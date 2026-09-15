package cloudformation

import (
	"bytes"
	"net/http"
	"net/url"
	"reflect"
	"testing"
)

// TestReadCFNJSONRequestRestoresQueryParameterStrings reproduces route66 #3923:
// the AWS Query dispatcher coerced ParameterValue "false" into a JSON bool, and
// CreateStack answered "Failed to parse request body" for the committed
// coredata-tables.json stack's LegacyBackupTags=false parameter.
func TestReadCFNJSONRequestRestoresQueryParameterStrings(t *testing.T) {
	t.Parallel()

	// Body and form mirror what QueryProtocolDispatcher hands the handler:
	// the coerced JSON body, with the original strings still on r.Form.
	body := []byte(`{"StackName":"local-test-CoreDataTables","Parameters":[` +
		`{"ParameterKey":"LegacyBackupTags","ParameterValue":false},` +
		`{"ParameterKey":"Width","ParameterValue":7}]}`)
	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Form = url.Values{
		"StackName":                          {"local-test-CoreDataTables"},
		"Parameters.member.1.ParameterKey":   {"LegacyBackupTags"},
		"Parameters.member.1.ParameterValue": {"false"},
		"Parameters.member.2.ParameterKey":   {"Width"},
		"Parameters.member.2.ParameterValue": {"007"},
	}

	var got CreateStackRequest
	if err := readCFNJSONRequest(req, &got); err != nil {
		t.Fatalf("readCFNJSONRequest: %v", err)
	}

	// The exact form spellings survive, including a leading-zero numeric.
	want := StackParameters{"LegacyBackupTags": "false", "Width": "007"}
	if !reflect.DeepEqual(got.Parameters, want) {
		t.Fatalf("parameters = %#v, want %#v", got.Parameters, want)
	}
}

// TestEvalFnIfNoValueDropsProperty reproduces the second #3923 gap: the
// coredata-tables.json Tags property is Fn::If over a condition with an
// AWS::NoValue branch, which the materializer rejected as unsupported.
func TestEvalFnIfNoValueDropsProperty(t *testing.T) {
	t.Parallel()

	ec := &evalCtx{}
	ec.params = map[string]string{"LegacyBackupTags": "false", "Prefix": "local-test."}
	ec.conditionDefs = map[string]any{
		"HasLegacyBackupTags": map[string]any{"Fn::Equals": []any{map[string]any{"Ref": "LegacyBackupTags"}, "true"}},
	}

	props := map[string]any{
		"TableName": map[string]any{"Fn::Sub": "${Prefix}cthweb.log_mail"},
		"Tags": map[string]any{"Fn::If": []any{
			"HasLegacyBackupTags",
			[]any{map[string]any{"Key": "enable-backups", "Value": "true"}},
			map[string]any{"Ref": "AWS::NoValue"},
		}},
	}

	got, err := ec.eval(props)
	if err != nil {
		t.Fatalf("eval: %v", err)
	}

	// The false branch removes Tags entirely rather than leaving "".
	want := map[string]any{"TableName": "local-test.cthweb.log_mail"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("eval = %#v, want %#v", got, want)
	}
}
