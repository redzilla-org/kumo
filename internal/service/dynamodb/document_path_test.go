package dynamodb

// This is the ONE test authored against the route66 fork's document-path
// grammar (GH #3669, see document_path.go's file header). tokenizeDocumentPath
// is the genuinely algorithmic piece of the fix -- it implements the DynamoDB
// developer guide's document-path grammar, element ('.' element | '[' N ']')*,
// cutting the raw expression text BEFORE any ExpressionAttributeNames alias is
// resolved (an alias value may itself contain '.', which is exactly why it is
// aliased, so resolving first would re-split a literal name into a bogus
// nested path -- see the file header's rationale). getDocumentPath,
// setDocumentPath and removeDocumentPath are plumbing walks over the resulting
// []pathElement and are proven end-to-end instead, against the real running
// fork binary (see scripts/debugging/inspect_local_session_item_gh3669.py in
// route66).
//
// Golden-rule check: this file is ~90 lines against document_path.go's 382.

import (
	"errors"
	"reflect"
	"testing"
)

// TestTokenizeDocumentPath proves the happy-path grammar: a path mixing
// aliased map keys, a list index, and a literal map key tokenizes into the
// right element sequence with alias tokens left UNRESOLVED (the "#a"/"#b"
// text is carried verbatim -- resolveDocumentPathTokens, not this function,
// substitutes ExpressionAttributeNames). Oracle: the DynamoDB developer
// guide's document-path grammar.
func TestTokenizeDocumentPath(t *testing.T) {
	t.Parallel()

	got, err := tokenizeDocumentPath("#a.#b[2].c")
	if err != nil {
		t.Fatalf("tokenizeDocumentPath(%q): unexpected error: %v", "#a.#b[2].c", err)
	}

	want := []documentPathToken{
		{text: "#a"},
		{text: "#b"},
		{index: 2, isIndex: true},
		{text: "c"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokenizeDocumentPath(%q) = %#v, want %#v", "#a.#b[2].c", got, want)
	}
}

// TestTokenizeDocumentPathRejectsMalformedPaths covers every way the grammar
// can be violated: a would-be empty element (consecutive/trailing dots), a
// non-numeric or negative list index, a path with no root element, and the
// empty string. Each must fail with the SAME ValidationException DynamoDB
// itself returns for an invalid update document path -- callers (SET/ADD/
// DELETE/REMOVE and the condition evaluator) all render this error verbatim.
func TestTokenizeDocumentPathRejectsMalformedPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty path", raw: ""},
		{name: "consecutive dots leave an empty element", raw: "a..b"},
		{name: "trailing dot promises an element never delivered", raw: "a."},
		{name: "non-numeric list index", raw: "a[x]"},
		{name: "negative list index", raw: "a[-1]"},
		{name: "no root element (path opens with an index)", raw: "[0].a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := tokenizeDocumentPath(tt.raw)
			if err == nil {
				t.Fatalf("tokenizeDocumentPath(%q): expected an error, got none", tt.raw)
			}

			var tableErr *TableError
			if !errors.As(err, &tableErr) || tableErr.Code != errCodeValidation {
				t.Fatalf("tokenizeDocumentPath(%q): expected ValidationException, got %v", tt.raw, err)
			}

			if tableErr.Message != invalidDocumentPathMessage {
				t.Fatalf("tokenizeDocumentPath(%q): error message = %q, want %q", tt.raw, tableErr.Message, invalidDocumentPathMessage)
			}
		})
	}
}
