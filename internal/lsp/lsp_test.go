// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lsp

import (
	"bytes"
	"context"
	"fmt"
	"go/token"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/packages/packagestest"
	"golang.org/x/tools/internal/lsp/protocol"
	"golang.org/x/tools/internal/lsp/source"
)

// TODO(rstambler): Remove this once Go 1.12 is released as we will end support
// for versions of Go <= 1.10.
var goVersion111 = true

func TestLSP(t *testing.T) {
	packagestest.TestAll(t, testLSP)
}

func testLSP(t *testing.T, exporter packagestest.Exporter) {
	const dir = "testdata"
	const expectedCompletionsCount = 43
	const expectedDiagnosticsCount = 14
	const expectedFormatCount = 3
	const expectedDefinitionsCount = 16

	files := packagestest.MustCopyFileTree(dir)
	for fragment, operation := range files {
		if trimmed := strings.TrimSuffix(fragment, ".in"); trimmed != fragment {
			delete(files, fragment)
			files[trimmed] = operation
		}
	}
	modules := []packagestest.Module{
		{
			Name:  "golang.org/x/tools/internal/lsp",
			Files: files,
		},
	}
	exported := packagestest.Export(t, exporter, modules)
	defer exported.Cleanup()

	// collect results for certain tests
	expectedDiagnostics := make(diagnostics)
	completionItems := make(completionItems)
	expectedCompletions := make(completions)
	expectedFormat := make(formats)
	expectedDefinitions := make(definitions)

	s := &server{
		view: source.NewView(),
	}
	// merge the config objects
	cfg := *exported.Config
	cfg.Fset = s.view.Config.Fset
	cfg.Mode = packages.LoadSyntax
	s.view.Config = &cfg

	// Do a first pass to collect special markers
	if err := exported.Expect(map[string]interface{}{
		"item": func(name string, r packagestest.Range, _, _ string) {
			exported.Mark(name, r)
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Collect any data that needs to be used by subsequent tests.
	if err := exported.Expect(map[string]interface{}{
		"diag":     expectedDiagnostics.collect,
		"item":     completionItems.collect,
		"complete": expectedCompletions.collect,
		"format":   expectedFormat.collect,
		"godef":    expectedDefinitions.collect,
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("Completion", func(t *testing.T) {
		t.Helper()
		if goVersion111 { // TODO(rstambler): Remove this when we no longer support Go 1.10.
			if len(expectedCompletions) != expectedCompletionsCount {
				t.Errorf("got %v completions expected %v", len(expectedCompletions), expectedCompletionsCount)
			}
		}
		expectedCompletions.test(t, exported, s, completionItems)
	})

	t.Run("Diagnostics", func(t *testing.T) {
		t.Helper()
		diagnosticsCount := expectedDiagnostics.test(t, exported, s.view)
		if goVersion111 { // TODO(rstambler): Remove this when we no longer support Go 1.10.
			if diagnosticsCount != expectedDiagnosticsCount {
				t.Errorf("got %v diagnostics expected %v", diagnosticsCount, expectedDiagnosticsCount)
			}
		}
	})

	t.Run("Format", func(t *testing.T) {
		t.Helper()
		if goVersion111 { // TODO(rstambler): Remove this when we no longer support Go 1.10.
			if len(expectedFormat) != expectedFormatCount {
				t.Errorf("got %v formats expected %v", len(expectedFormat), expectedFormatCount)
			}
		}
		expectedFormat.test(t, s)
	})

	t.Run("Definitions", func(t *testing.T) {
		t.Helper()
		if goVersion111 { // TODO(rstambler): Remove this when we no longer support Go 1.10.
			if len(expectedDefinitions) != expectedDefinitionsCount {
				t.Errorf("got %v definitions expected %v", len(expectedDefinitions), expectedDefinitionsCount)
			}
		}
		expectedDefinitions.test(t, s)
	})
}

type diagnostics map[string][]protocol.Diagnostic
type completionItems map[token.Pos]*protocol.CompletionItem
type completions map[token.Position][]token.Pos
type formats map[string]string
type definitions map[protocol.Location]protocol.Location

func (c completions) test(t *testing.T, exported *packagestest.Exported, s *server, items completionItems) {
	for src, itemList := range c {
		var want []protocol.CompletionItem
		for _, pos := range itemList {
			want = append(want, *items[pos])
		}
		list, err := s.Completion(context.Background(), &protocol.CompletionParams{
			TextDocumentPositionParams: protocol.TextDocumentPositionParams{
				TextDocument: protocol.TextDocumentIdentifier{
					URI: protocol.DocumentURI(source.ToURI(src.Filename)),
				},
				Position: protocol.Position{
					Line:      float64(src.Line - 1),
					Character: float64(src.Column - 1),
				},
			},
		})
		if err != nil {
			t.Fatalf("completion failed for %s:%v:%v: %v", filepath.Base(src.Filename), src.Line, src.Column, err)
		}
		got := list.Items
		if equal := reflect.DeepEqual(want, got); !equal {
			t.Errorf(diffC(src, want, got))
		}
	}
}

func (c completions) collect(src token.Position, expected []token.Pos) {
	c[src] = expected
}

func (i completionItems) collect(pos token.Pos, label, detail, kind string) {
	var k protocol.CompletionItemKind
	switch kind {
	case "struct":
		k = protocol.StructCompletion
	case "func":
		k = protocol.FunctionCompletion
	case "var":
		k = protocol.VariableCompletion
	case "type":
		k = protocol.TypeParameterCompletion
	case "field":
		k = protocol.FieldCompletion
	case "interface":
		k = protocol.InterfaceCompletion
	case "const":
		k = protocol.ConstantCompletion
	case "method":
		k = protocol.MethodCompletion
	case "package":
		k = protocol.ModuleCompletion
	}
	i[pos] = &protocol.CompletionItem{
		Label:  label,
		Detail: detail,
		Kind:   float64(k),
	}
}

func (d diagnostics) test(t *testing.T, exported *packagestest.Exported, v *source.View) int {
	count := 0
	for filename, want := range d {
		f := v.GetFile(source.ToURI(filename))
		sourceDiagnostics, err := source.Diagnostics(context.Background(), v, f)
		if err != nil {
			t.Fatal(err)
		}
		got := toProtocolDiagnostics(v, sourceDiagnostics[filename])
		sorted(got)
		if equal := reflect.DeepEqual(want, got); !equal {
			t.Error(diffD(filename, want, got))
		}
		count += len(want)
	}
	return count
}

func (d diagnostics) collect(pos token.Position, msg string) {
	if _, ok := d[pos.Filename]; !ok {
		d[pos.Filename] = []protocol.Diagnostic{}
	}
	// If a file has an empty diagnostics, mark that and return. This allows us
	// to avoid testing diagnostics in files that may have a lot of them.
	if msg == "" {
		return
	}
	line := float64(pos.Line - 1)
	col := float64(pos.Column - 1)
	want := protocol.Diagnostic{
		Range: protocol.Range{
			Start: protocol.Position{
				Line:      line,
				Character: col,
			},
			End: protocol.Position{
				Line:      line,
				Character: col,
			},
		},
		Severity: protocol.SeverityError,
		Source:   "LSP",
		Message:  msg,
	}
	d[pos.Filename] = append(d[pos.Filename], want)
}

func (f formats) test(t *testing.T, s *server) {
	for filename, gofmted := range f {
		edits, err := s.Formatting(context.Background(), &protocol.DocumentFormattingParams{
			TextDocument: protocol.TextDocumentIdentifier{
				URI: protocol.DocumentURI(source.ToURI(filename)),
			},
		})
		if err != nil || len(edits) == 0 {
			if gofmted != "" {
				t.Error(err)
			}
			continue
		}
		edit := edits[0]
		if edit.NewText != gofmted {
			t.Errorf("formatting failed: (got: %s), (expected: %s)", edit.NewText, gofmted)
		}
	}
}

func (f formats) collect(pos token.Position) {
	cmd := exec.Command("gofmt", pos.Filename)
	stdout := bytes.NewBuffer(nil)
	cmd.Stdout = stdout
	cmd.Run() // ignore error, sometimes we have intentionally ungofmt-able files
	f[pos.Filename] = stdout.String()
}

func (d definitions) test(t *testing.T, s *server) {
	for src, target := range d {
		locs, err := s.Definition(context.Background(), &protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{
				URI: src.URI,
			},
			Position: src.Range.Start,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(locs) != 1 {
			t.Errorf("got %d locations for definition, expected 1", len(locs))
		}
		if locs[0] != target {
			t.Errorf("for %v got %v want %v", src, locs[0], target)
		}
	}
}

func (d definitions) collect(fset *token.FileSet, src, target packagestest.Range) {
	sRange := source.Range{Start: src.Start, End: src.End}
	sLoc := toProtocolLocation(fset, sRange)
	tRange := source.Range{Start: target.Start, End: target.End}
	tLoc := toProtocolLocation(fset, tRange)
	d[sLoc] = tLoc
}

// diffD prints the diff between expected and actual diagnostics test results.
func diffD(filename string, want, got []protocol.Diagnostic) string {
	msg := &bytes.Buffer{}
	fmt.Fprintf(msg, "diagnostics failed for %s:\nexpected:\n", filename)
	for _, d := range want {
		fmt.Fprintf(msg, "  %v\n", d)
	}
	fmt.Fprintf(msg, "got:\n")
	for _, d := range got {
		fmt.Fprintf(msg, "  %v\n", d)
	}
	return msg.String()
}

// diffC prints the diff between expected and actual completion test results.
func diffC(pos token.Position, want, got []protocol.CompletionItem) string {
	msg := &bytes.Buffer{}
	fmt.Fprintf(msg, "completion failed for %s:%v:%v:\nexpected:\n", filepath.Base(pos.Filename), pos.Line, pos.Column)
	for _, d := range want {
		fmt.Fprintf(msg, "  %v\n", d)
	}
	fmt.Fprintf(msg, "got:\n")
	for _, d := range got {
		fmt.Fprintf(msg, "  %v\n", d)
	}
	return msg.String()
}
