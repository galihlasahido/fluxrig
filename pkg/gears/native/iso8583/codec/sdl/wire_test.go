// Copyright (c) 2026 JAAB Tech SAS, Uruguay
// SPDX-License-Identifier: Apache-2.0

package sdl

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moov-io/iso8583/specs"
)

const refSpec = "../../../../../../examples/specs/iso8583-v87-ascii.yaml"

// The acceptance criterion for adopting Moov's wire vocabulary: removing
// fluxrig's semantic keys must leave a document Moov accepts on its own. If this
// fails, the two layers have fused again and the wire half is no longer a
// commodity we consume.
func TestStripTestYieldsAMoovDocument(t *testing.T) {
	data, err := os.ReadFile(refSpec)
	if err != nil {
		t.Fatalf("read reference spec: %v", err)
	}

	wire, err := WireDocument(data, filepath.Dir(refSpec))
	if err != nil {
		t.Fatalf("derive wire document: %v", err)
	}
	moovSpec, err := specs.ImportYAML(wire)
	if err != nil {
		t.Fatalf("moov rejected the stripped document: %v", err)
	}

	full, _, err := LoadSpec(refSpec)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	if len(moovSpec.Fields) != len(full.Fields) {
		t.Errorf("stripped document has %d fields, the compiled spec %d",
			len(moovSpec.Fields), len(full.Fields))
	}
	if len(full.Fields) < 60 {
		t.Errorf("compiled %d fields, expected the full ISO 8583:87 set", len(full.Fields))
	}
}

// Stripping must remove the semantic keys and nothing else. A wire key lost here
// is a field that parses differently, which no test downstream would attribute
// to the stripper.
func TestStripKeepsWireKeysAndDropsSemanticOnes(t *testing.T) {
	in := map[string]any{
		"name": "PAN", "type": "String", "length": 19,
		"enc": "ASCII", "prefix": "ASCII.LL",
		"padding":     map[string]any{"type": "Left", "pad": "0"},
		"alias":       "card.pan",
		"sensitivity": "pan",
		"log_mask":    true,
		"scope":       "public",
		"description": "Primary account number.",
	}
	out, err := stripSemantic(in)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}

	for _, k := range []string{"type", "length", "enc", "prefix", "padding"} {
		if _, ok := out[k]; !ok {
			t.Errorf("wire key %q was dropped", k)
		}
	}
	for _, k := range []string{"alias", "sensitivity", "log_mask", "scope"} {
		if _, ok := out[k]; ok {
			t.Errorf("semantic key %q survived", k)
		}
	}
	// name is not dropped but translated: Moov calls this description.
	if out["description"] != "PAN" {
		t.Errorf("label lost in translation: got %v, want PAN", out["description"])
	}
}

// The classification is worth carrying only if it drives behaviour. A field
// marked `pan` must be treated as secure without also having to remember
// log_mask, because forgetting one of the two is how a PAN reaches a log.
func TestSensitivityImpliesSecureWithoutLogMask(t *testing.T) {
	_, meta, err := LoadSpec(refSpec)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	if len(meta.SecureIDs) == 0 {
		t.Fatal("no field was classified secure")
	}
	if !meta.SecureIDs[2] {
		t.Error("DE 2 (PAN) is not secure")
	}
	if len(meta.Aliases) < 10 {
		t.Errorf("only %d aliases resolved; the semantic layer did not load", len(meta.Aliases))
	}
}

// The point of naming a base instead of restating it: the wire layer is whatever
// the linked library says it is, and only the declared differences are ours.
func TestWireBaseIsConsumedNotCopied(t *testing.T) {
	data, err := os.ReadFile(refSpec)
	if err != nil {
		t.Fatalf("read reference spec: %v", err)
	}
	if bytes.Contains(data, []byte("\n  fields:\n    0:\n      type:")) {
		t.Error("the spec restates wire attributes under spec.fields")
	}

	spec, _, err := LoadSpec(refSpec)
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	base := specs.Spec87ASCII

	// Every field the base defines survives; the overlay only adds.
	for id := range base.Fields {
		if _, ok := spec.Fields[id]; !ok {
			t.Errorf("field %d present in the base is missing after the overlay", id)
		}
	}
	// And the overlay's own additions are there.
	for _, id := range []int{70, 104} {
		if _, ok := spec.Fields[id]; !ok {
			t.Errorf("field %d declared by the overlay is missing", id)
		}
	}
	if len(spec.Fields) <= len(base.Fields) {
		t.Errorf("compiled %d fields, base has %d; the overlay added nothing",
			len(spec.Fields), len(base.Fields))
	}
}

// Layer ownership is enforced, not just documented. A wire key on a semantic
// entry must fail at load rather than quietly winning over the wire document.
func TestSemanticLayerMayNotCarryWireKeys(t *testing.T) {
	doc := []byte(`
spec:
  id: t
  wire:
    source: "moov:spec87ascii"
  fields:
    2:
      alias: card.pan
      length: 19
`)
	tmp := t.TempDir() + "/spec.yaml"
	if err := os.WriteFile(tmp, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := LoadSpec(tmp)
	if err == nil {
		t.Fatal("a wire key under spec.fields was accepted")
	}
	if !strings.Contains(err.Error(), "length") || !strings.Contains(err.Error(), "spec.wire.fields") {
		t.Errorf("error should name the key and where it belongs: %v", err)
	}
}

// A file wire source is read from the spec's own directory and nowhere else. The
// symlink cases are the reason the read goes through os.Root: the path names a
// file inside the directory, and only the OS can see that it leads outside.
func TestFileWireSourceStaysInsideSpecDirectory(t *testing.T) {
	const wireDoc = "name: base\nfields:\n  \"0\":\n    type: String\n    length: 4\n"

	top := t.TempDir()
	specDir := filepath.Join(top, "spec")
	outside := filepath.Join(top, "outside")
	for _, d := range []string{filepath.Join(specDir, "wire"), outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(specDir, "wire", "base.yaml")
	secret := filepath.Join(outside, "secret.yaml")
	for _, f := range []string{inside, secret} {
		if err := os.WriteFile(f, []byte(wireDoc), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("relative source inside the directory loads", func(t *testing.T) {
		fields, name, err := baseWireFields("wire/base.yaml", specDir)
		if err != nil {
			t.Fatalf("a source inside the spec's directory was refused: %v", err)
		}
		if name != "base" || len(fields) != 1 {
			t.Errorf("got name %q and %d fields, want base and 1", name, len(fields))
		}
	})

	refused := []struct {
		name   string
		source string
		want   string
	}{
		{"parent directory", "../outside/secret.yaml", "resolves outside the spec's directory"},
		{"parent after a subdirectory", "wire/../../outside/secret.yaml", "resolves outside the spec's directory"},
		{"absolute path", secret, "must be relative to the spec"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := baseWireFields(tc.source, specDir)
			if err == nil {
				t.Fatalf("source %q was read", tc.source)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should say %q: %v", tc.want, err)
			}
		})
	}

	t.Run("no spec directory", func(t *testing.T) {
		_, _, err := baseWireFields("wire/base.yaml", "")
		if err == nil || !strings.Contains(err.Error(), "resolved from the store rather than from disk") {
			t.Errorf("a path source without a spec directory was not refused clearly: %v", err)
		}
	})

	t.Run("symlink that stays inside loads", func(t *testing.T) {
		link := filepath.Join(specDir, "inner.yaml")
		if err := os.Symlink(filepath.Join("wire", "base.yaml"), link); err != nil {
			t.Skipf("symlinks are not available here: %v", err)
		}
		if _, _, err := baseWireFields("inner.yaml", specDir); err != nil {
			t.Fatalf("a symlink to a file inside the spec's directory was refused: %v", err)
		}
	})

	t.Run("symlinked file pointing outside", func(t *testing.T) {
		link := filepath.Join(specDir, "link.yaml")
		if err := os.Symlink(secret, link); err != nil {
			t.Skipf("symlinks are not available here: %v", err)
		}
		if _, _, err := baseWireFields("link.yaml", specDir); err == nil {
			t.Fatal("a symlink inside the spec's directory was followed to a file outside it")
		}
	})

	t.Run("symlinked directory pointing outside", func(t *testing.T) {
		link := filepath.Join(specDir, "linkdir")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlinks are not available here: %v", err)
		}
		if _, _, err := baseWireFields("linkdir/secret.yaml", specDir); err == nil {
			t.Fatal("a symlinked directory inside the spec's directory was followed outside it")
		}
	})
}
