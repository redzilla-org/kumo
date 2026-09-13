package cloudformation

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestCreateStackRequestUnmarshalsQueryParameters protects the AWS Query
// dispatcher contract: Parameters.member.N fields arrive at the service as a
// JSON array, including explicitly supplied empty parameter values.
func TestCreateStackRequestUnmarshalsQueryParameters(t *testing.T) {
	t.Parallel()

	// The payload mirrors formToJSON's normalized output for two AWS Query
	// Parameters.member.N structures rather than the legacy direct-JSON shape.
	payload := []byte(`{
		"StackName":"example",
		"Parameters":[
			{"ParameterKey":"EnvironmentName","ParameterValue":"test"},
			{"ParameterKey":"OptionalSuffix","ParameterValue":""}
		]
	}`)

	var request CreateStackRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatalf("unmarshal AWS Query-normalized CreateStack request: %v", err)
	}

	// Map equality proves normalization retained both keys and did not confuse
	// an explicit empty value with an absent ParameterValue field.
	want := StackParameters{
		"EnvironmentName": "test",
		"OptionalSuffix":  "",
	}
	if !reflect.DeepEqual(request.Parameters, want) {
		t.Fatalf("parameters = %#v, want %#v", request.Parameters, want)
	}
}

// TestStackParametersUnmarshalsLegacyMap keeps existing direct JSON callers
// compatible while the AWS Query representation uses the new array path.
func TestStackParametersUnmarshalsLegacyMap(t *testing.T) {
	t.Parallel()

	var parameters StackParameters
	if err := json.Unmarshal([]byte(`{"EnvironmentName":"legacy","OptionalSuffix":""}`), &parameters); err != nil {
		t.Fatalf("unmarshal legacy parameter map: %v", err)
	}

	// The legacy path must preserve the same internal lookup representation as
	// the Query path so materialization code needs no protocol-specific branch.
	want := StackParameters{"EnvironmentName": "legacy", "OptionalSuffix": ""}
	if !reflect.DeepEqual(parameters, want) {
		t.Fatalf("parameters = %#v, want %#v", parameters, want)
	}
}

// TestStackParametersRejectsInvalidInput pins the fail-fast boundary that
// prevents malformed or ambiguous parameter entries reaching stack creation.
func TestStackParametersRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	// Each case represents a distinct invalid wire contract; in particular,
	// duplicate keys must not silently overwrite an earlier Query member.
	tests := []struct {
		name    string
		payload string
	}{
		{name: "non-container shape", payload: `"EnvironmentName=test"`},
		{name: "malformed JSON", payload: `[{"ParameterKey":"EnvironmentName"`},
		{name: "empty key", payload: `[{"ParameterKey":"","ParameterValue":"test"}]`},
		{name: "missing key", payload: `[{"ParameterValue":"test"}]`},
		{name: "missing value", payload: `[{"ParameterKey":"EnvironmentName"}]`},
		{name: "duplicate key", payload: `[
			{"ParameterKey":"EnvironmentName","ParameterValue":"first"},
			{"ParameterKey":"EnvironmentName","ParameterValue":"second"}
		]`},
	}

	// Error-only assertions intentionally avoid coupling the contract to error
	// prose while requiring every invalid representation to be rejected.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var parameters StackParameters
			if err := json.Unmarshal([]byte(test.payload), &parameters); err == nil {
				t.Fatalf("json.Unmarshal(%s) unexpectedly succeeded with %#v", test.payload, parameters)
			}
		})
	}
}
