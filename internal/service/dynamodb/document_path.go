package dynamodb

// route66 fork (GH #3669, local-verify run 140): kumo's UpdateExpression
// implementation treated a document path as a single flat attribute name, so
// "SET #attrs.#n0 = :v0" with #attrs=attributes and #n0=attr_a created a NEW
// top-level attribute literally called "attributes.attr_a" and left the
// "attributes" map empty; "REMOVE #attrs.#n0" removed nothing. The webapp's
// session store writes every session attribute through exactly that shape, so
// against the emulator every session attribute silently vanished. This file is
// the single document-path implementation that every update action (SET, ADD,
// DELETE, REMOVE) and the condition evaluator route through.
//
// Tokenizing comes FIRST and alias resolution SECOND, deliberately: an
// ExpressionAttributeNames entry may itself contain a '.' (avoiding an illegal
// literal name is precisely why such an alias exists), so splitting the
// name-substituted string on '.' would re-cut a literal name into a bogus
// nested path. While the path is being cut into elements, "#alias" is an opaque
// token; each element is resolved to its real name only afterwards.

import (
	"strconv"
	"strings"
)

// invalidDocumentPathMessage is DynamoDB's verbatim ValidationException text for
// a path that cannot be resolved for update. Real DynamoDB does NOT auto-create
// missing intermediate map/list elements — it rejects the whole request — and
// the emulator matches that so a caller sees the same failure locally as in the
// cloud instead of a silently different item shape.
const invalidDocumentPathMessage = "The document path provided in the update expression is invalid for update"

// errInvalidDocumentPath is the one failure every malformed or unresolvable
// document path produces. It is a TableError, so the UpdateItem handler already
// renders it as a 400 ValidationException with this exact message.
func errInvalidDocumentPath() error {
	return newValidationException(invalidDocumentPathMessage)
}

// pathElement is one resolved step of a document path: a map key (name) or a
// list index. DynamoDB's grammar is element ('.' element | '[' N ']')*.
type pathElement struct {
	name    string
	index   int
	isIndex bool
}

// documentPathToken is one step of a path that has been cut out of the raw
// expression but NOT yet resolved: text is either a literal name or an
// unresolved "#alias".
type documentPathToken struct {
	text    string
	index   int
	isIndex bool
}

// parseDocumentPath cuts raw into elements and then resolves each "#alias"
// element through exprNames. exprNames may be nil when the caller has already
// substituted names (the condition evaluator does), in which case any remaining
// "#alias" is an undefined name and fails the path.
func parseDocumentPath(raw string, exprNames map[string]string) ([]pathElement, error) {
	tokens, err := tokenizeDocumentPath(raw)
	if err != nil {
		return nil, err
	}

	return resolveDocumentPathTokens(tokens, exprNames)
}

// tokenizeDocumentPath splits a raw path into tokens without consulting
// ExpressionAttributeNames, so an alias whose value contains '.' or '[' stays a
// single element. A name element runs until the next '.' or '[': DynamoDB
// forbids those characters in an unaliased name in an expression, which is what
// makes this cut unambiguous.
func tokenizeDocumentPath(raw string) ([]documentPathToken, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return nil, errInvalidDocumentPath()
	}

	var tokens []documentPathToken

	i := 0
	expectName := true

	for i < len(path) {
		switch {
		case expectName:
			start := i
			for i < len(path) && path[i] != '.' && path[i] != '[' && path[i] != ']' {
				i++
			}

			if i == start {
				return nil, errInvalidDocumentPath()
			}

			tokens = append(tokens, documentPathToken{text: path[start:i]})
			expectName = false

		case path[i] == '.':
			i++
			expectName = true

		case path[i] == '[':
			closing := strings.IndexByte(path[i:], ']')
			if closing < 0 {
				return nil, errInvalidDocumentPath()
			}

			digits := path[i+1 : i+closing]

			index, err := strconv.Atoi(digits)
			if err != nil || index < 0 {
				return nil, errInvalidDocumentPath()
			}

			tokens = append(tokens, documentPathToken{index: index, isIndex: true})
			i += closing + 1

		default:
			// Only '.' and '[' may follow a completed element; anything else
			// (a stray ']', for instance) is a malformed path.
			return nil, errInvalidDocumentPath()
		}
	}

	// A trailing '.' leaves an element promised but never delivered.
	if expectName {
		return nil, errInvalidDocumentPath()
	}

	return tokens, nil
}

// resolveDocumentPathTokens maps each name token through ExpressionAttributeNames.
// An undefined alias fails the path rather than being written literally, which
// is what produced the "#attrs" style garbage attribute names before #3669.
func resolveDocumentPathTokens(tokens []documentPathToken, exprNames map[string]string) ([]pathElement, error) {
	elements := make([]pathElement, 0, len(tokens))

	for _, token := range tokens {
		if token.isIndex {
			elements = append(elements, pathElement{index: token.index, isIndex: true})

			continue
		}

		name := token.text

		if strings.HasPrefix(name, "#") {
			resolved, ok := exprNames[name]
			if !ok || resolved == "" {
				return nil, errInvalidDocumentPath()
			}

			name = resolved
		}

		elements = append(elements, pathElement{name: name})
	}

	// A path always starts at a top-level attribute name; "[0].x" has no root.
	if len(elements) == 0 || elements[0].isIndex {
		return nil, errInvalidDocumentPath()
	}

	return elements, nil
}

// getDocumentPath reads the value at path, reporting whether it exists. A
// missing or wrongly typed intermediate is simply "does not exist": reads are
// never an error in DynamoDB (a condition on an absent attribute is false, not a
// failure).
func getDocumentPath(item Item, path []pathElement) (AttributeValue, bool) {
	if len(path) == 0 || path[0].isIndex {
		return AttributeValue{}, false
	}

	current, ok := item[path[0].name]
	if !ok {
		return AttributeValue{}, false
	}

	for _, element := range path[1:] {
		if element.isIndex {
			if element.index >= len(current.L) || current.L[element.index] == nil {
				return AttributeValue{}, false
			}

			current = *current.L[element.index]

			continue
		}

		child, found := current.M[element.name]
		if !found || child == nil {
			return AttributeValue{}, false
		}

		current = *child
	}

	return current, true
}

// setDocumentPath writes value at path, mutating item in place. A single-element
// path is a plain top-level assignment (creating the attribute if absent), which
// keeps pre-#3669 behavior byte-identical. Deeper paths require every
// intermediate element to already exist and to be of the right container type,
// matching DynamoDB, which returns a ValidationException rather than
// materializing parents.
//
//nolint:gocritic // hugeParam: AttributeValue passed by value to match the surrounding code.
func setDocumentPath(item Item, path []pathElement, value AttributeValue) error {
	if len(path) == 0 || path[0].isIndex {
		return errInvalidDocumentPath()
	}

	root := path[0].name

	if len(path) == 1 {
		item[root] = value

		return nil
	}

	current, ok := item[root]
	if !ok {
		return errInvalidDocumentPath()
	}

	updated, err := setNestedPath(current, path[1:], value)
	if err != nil {
		return err
	}

	// Written back because a list append changes the slice header; map writes
	// already mutate the shared map, so this is a no-op for those.
	item[root] = updated

	return nil
}

// setNestedPath returns current with value written at rest. The updated value is
// returned rather than mutated through a pointer so that a list append (which
// reallocates) propagates to the parent.
//
//nolint:gocritic // hugeParam: AttributeValue passed by value to match the surrounding code.
func setNestedPath(current AttributeValue, rest []pathElement, value AttributeValue) (AttributeValue, error) {
	element := rest[0]

	if element.isIndex {
		if current.L == nil {
			return current, errInvalidDocumentPath()
		}

		if len(rest) == 1 {
			// DynamoDB appends when the index is past the end of the list.
			if element.index >= len(current.L) {
				stored := value
				current.L = append(current.L, &stored)

				return current, nil
			}

			stored := value
			current.L[element.index] = &stored

			return current, nil
		}

		if element.index >= len(current.L) || current.L[element.index] == nil {
			return current, errInvalidDocumentPath()
		}

		child, err := setNestedPath(*current.L[element.index], rest[1:], value)
		if err != nil {
			return current, err
		}

		current.L[element.index] = &child

		return current, nil
	}

	if current.M == nil {
		return current, errInvalidDocumentPath()
	}

	if len(rest) == 1 {
		stored := value
		current.M[element.name] = &stored

		return current, nil
	}

	next, ok := current.M[element.name]
	if !ok || next == nil {
		return current, errInvalidDocumentPath()
	}

	child, err := setNestedPath(*next, rest[1:], value)
	if err != nil {
		return current, err
	}

	current.M[element.name] = &child

	return current, nil
}

// removeDocumentPath deletes the element at path, mutating item in place.
// Removing a path that does not exist is a no-op, per DynamoDB.
func removeDocumentPath(item Item, path []pathElement) {
	if len(path) == 0 || path[0].isIndex {
		return
	}

	root := path[0].name

	if len(path) == 1 {
		delete(item, root)

		return
	}

	current, ok := item[root]
	if !ok {
		return
	}

	// Written back because removing a list element reallocates the slice.
	item[root] = removeNestedPath(current, path[1:])
}

//nolint:gocritic // hugeParam: AttributeValue passed by value to match the surrounding code.
func removeNestedPath(current AttributeValue, rest []pathElement) AttributeValue {
	element := rest[0]

	if element.isIndex {
		if element.index >= len(current.L) {
			return current
		}

		if len(rest) == 1 {
			// Removing a list element shifts the remaining elements down.
			current.L = append(current.L[:element.index], current.L[element.index+1:]...)

			return current
		}

		if current.L[element.index] == nil {
			return current
		}

		child := removeNestedPath(*current.L[element.index], rest[1:])
		current.L[element.index] = &child

		return current
	}

	next, ok := current.M[element.name]
	if !ok {
		return current
	}

	if len(rest) == 1 {
		delete(current.M, element.name)

		return current
	}

	if next == nil {
		return current
	}

	child := removeNestedPath(*next, rest[1:])
	current.M[element.name] = &child

	return current
}
