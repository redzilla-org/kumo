package cloudformation

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Error codes for CloudFormation.
const (
	errInvalidParameter = "ValidationError"
	errInternalError    = "InternalServiceError"
	errInvalidAction    = "InvalidAction"
)

// CreateStack handles the CreateStack action.
func (s *Service) CreateStack(w http.ResponseWriter, r *http.Request) {
	var req CreateStackRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		// route66 #3923: the cause is appended because a bare "Failed to parse
		// request body" hid which field the decoder rejected.
		writeCFNError(w, errInvalidParameter, "Failed to parse request body: "+err.Error(), http.StatusBadRequest)

		return
	}

	if req.StackName == "" {
		writeCFNError(w, errInvalidParameter, "StackName is required", http.StatusBadRequest)

		return
	}

	stack, err := s.storage.CreateStack(r.Context(), &req)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	// route66 fork: actually materialize the template's resources into the
	// sibling service stores. A failure rolls the stack record back and
	// fails the API call loudly — never a green stack over missing resources.
	if err := materializeStack(r.Context(), &req, stack, resolveRegion(r)); err != nil {
		_ = s.storage.DeleteStack(r.Context(), req.StackName)

		writeCFNError(w, errInvalidParameter, "materialization failed: "+err.Error(), http.StatusBadRequest)

		return
	}

	writeCFNXMLResponse(w, XMLCreateStackResponse{
		Xmlns: cfnXMLNS,
		Result: XMLCreateStackResult{
			StackID: stack.StackID,
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DeleteStack handles the DeleteStack action.
func (s *Service) DeleteStack(w http.ResponseWriter, r *http.Request) {
	var req DeleteStackRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.StackName == "" {
		writeCFNError(w, errInvalidParameter, "StackName is required", http.StatusBadRequest)

		return
	}

	// route66 fork: tear down materialized resources before dropping the
	// stack record. Resources are fetched first because DeleteStack removes
	// the record. Teardown failure fails the API call — no silent leaks.
	resources, resErr := s.storage.DescribeStackResources(r.Context(), req.StackName, "")
	if resErr != nil {
		handleCFNError(w, resErr)

		return
	}

	if err := teardownStack(r.Context(), resources); err != nil {
		writeCFNError(w, errInternalError, "teardown failed: "+err.Error(), http.StatusInternalServerError)

		return
	}

	err := s.storage.DeleteStack(r.Context(), req.StackName)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	writeCFNXMLResponse(w, XMLDeleteStackResponse{
		Xmlns:            cfnXMLNS,
		Result:           XMLDeleteStackResult{},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DescribeStacks handles the DescribeStacks action.
func (s *Service) DescribeStacks(w http.ResponseWriter, r *http.Request) {
	var req DescribeStacksRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	stacks, err := s.storage.DescribeStacks(r.Context(), req.StackName)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	xmlStacks := make([]XMLStack, 0, len(stacks))

	for _, stack := range stacks {
		xmlStacks = append(xmlStacks, convertToXMLStack(stack))
	}

	writeCFNXMLResponse(w, XMLDescribeStacksResponse{
		Xmlns: cfnXMLNS,
		Result: XMLDescribeStacksResult{
			Stacks: XMLStacks{Members: xmlStacks},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// ListStacks handles the ListStacks action.
func (s *Service) ListStacks(w http.ResponseWriter, r *http.Request) {
	var req ListStacksRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	stacks, err := s.storage.ListStacks(r.Context(), req.StackStatusFilter)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	xmlSummaries := make([]XMLStackSummary, 0, len(stacks))

	for _, stack := range stacks {
		xmlSummaries = append(xmlSummaries, convertToXMLStackSummary(stack))
	}

	writeCFNXMLResponse(w, XMLListStacksResponse{
		Xmlns: cfnXMLNS,
		Result: XMLListStacksResult{
			StackSummaries: XMLStackSummaries{Members: xmlSummaries},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// UpdateStack handles the UpdateStack action.
func (s *Service) UpdateStack(w http.ResponseWriter, r *http.Request) {
	var req UpdateStackRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.StackName == "" {
		writeCFNError(w, errInvalidParameter, "StackName is required", http.StatusBadRequest)

		return
	}

	stack, err := s.storage.UpdateStack(r.Context(), &req)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	writeCFNXMLResponse(w, XMLUpdateStackResponse{
		Xmlns: cfnXMLNS,
		Result: XMLUpdateStackResult{
			StackID: stack.StackID,
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DescribeStackResources handles the DescribeStackResources action.
func (s *Service) DescribeStackResources(w http.ResponseWriter, r *http.Request) {
	var req DescribeStackResourcesRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.StackName == "" {
		writeCFNError(w, errInvalidParameter, "StackName is required", http.StatusBadRequest)

		return
	}

	resources, err := s.storage.DescribeStackResources(r.Context(), req.StackName, req.LogicalResourceID)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	xmlResources := make([]XMLStackResource, 0, len(resources))

	for _, resource := range resources {
		xmlResources = append(xmlResources, convertToXMLStackResource(resource))
	}

	writeCFNXMLResponse(w, XMLDescribeStackResourcesResponse{
		Xmlns: cfnXMLNS,
		Result: XMLDescribeStackResourcesResult{
			StackResources: XMLStackResources{Members: xmlResources},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// GetTemplate handles the GetTemplate action.
func (s *Service) GetTemplate(w http.ResponseWriter, r *http.Request) {
	var req GetTemplateRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.StackName == "" {
		writeCFNError(w, errInvalidParameter, "StackName is required", http.StatusBadRequest)

		return
	}

	template, err := s.storage.GetTemplate(r.Context(), req.StackName)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	writeCFNXMLResponse(w, XMLGetTemplateResponse{
		Xmlns: cfnXMLNS,
		Result: XMLGetTemplateResult{
			TemplateBody: template,
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// ValidateTemplate handles the ValidateTemplate action.
func (s *Service) ValidateTemplate(w http.ResponseWriter, r *http.Request) {
	var req ValidateTemplateRequest
	if err := readCFNJSONRequest(r, &req); err != nil {
		writeCFNError(w, errInvalidParameter, "Failed to parse request body", http.StatusBadRequest)

		return
	}

	if req.TemplateBody == "" && req.TemplateURL == "" {
		writeCFNError(w, errInvalidParameter, "Either TemplateBody or TemplateURL is required", http.StatusBadRequest)

		return
	}

	result, err := s.storage.ValidateTemplate(r.Context(), req.TemplateBody)
	if err != nil {
		handleCFNError(w, err)

		return
	}

	xmlParams := make([]XMLTemplateParameter, 0, len(result.Parameters))

	for _, param := range result.Parameters {
		xmlParams = append(xmlParams, XMLTemplateParameter{
			ParameterKey:  param.ParameterKey,
			DefaultValue:  param.DefaultValue,
			NoEcho:        param.NoEcho,
			Description:   param.Description,
			ParameterType: param.ParameterType,
		})
	}

	writeCFNXMLResponse(w, XMLValidateTemplateResponse{
		Xmlns: cfnXMLNS,
		Result: XMLValidateTemplateResult{
			Parameters:   XMLTemplateParameters{Members: xmlParams},
			Description:  result.Description,
			Capabilities: XMLCapabilities{Members: result.Capabilities},
		},
		ResponseMetadata: XMLResponseMetadata{RequestID: uuid.New().String()},
	})
}

// DispatchAction routes the request to the appropriate handler based on Action parameter.
func (s *Service) DispatchAction(w http.ResponseWriter, r *http.Request) {
	action := extractAction(r)
	handler := s.getActionHandler(action)

	if handler == nil {
		writeCFNError(w, errInvalidAction, fmt.Sprintf("The action '%s' is not valid", action), http.StatusBadRequest)

		return
	}

	handler(w, r)
}

// getActionHandler returns the handler function for the given action.
func (s *Service) getActionHandler(action string) func(http.ResponseWriter, *http.Request) {
	handlers := map[string]func(http.ResponseWriter, *http.Request){
		"CreateStack":            s.CreateStack,
		"DeleteStack":            s.DeleteStack,
		"DescribeStacks":         s.DescribeStacks,
		"ListStacks":             s.ListStacks,
		"UpdateStack":            s.UpdateStack,
		"DescribeStackResources": s.DescribeStackResources,
		"GetTemplate":            s.GetTemplate,
		"ValidateTemplate":       s.ValidateTemplate,
	}

	return handlers[action]
}

// Helper functions.

// convertToXMLStack converts a Stack to XMLStack.
func convertToXMLStack(stack *Stack) XMLStack {
	params := make([]XMLParameter, 0, len(stack.Parameters))

	for key, value := range stack.Parameters {
		params = append(params, XMLParameter{
			ParameterKey:   key,
			ParameterValue: value,
		})
	}

	xmlStack := XMLStack{
		StackID:           stack.StackID,
		StackName:         stack.StackName,
		StackStatus:       stack.StackStatus,
		StackStatusReason: stack.StackStatusReason,
		CreationTime:      stack.CreationTime.Format("2006-01-02T15:04:05.000Z"),
		Parameters:        XMLParameters{Members: params},
	}

	if !stack.LastUpdatedTime.IsZero() {
		xmlStack.LastUpdatedTime = stack.LastUpdatedTime.Format("2006-01-02T15:04:05.000Z")
	}

	if !stack.DeletionTime.IsZero() {
		xmlStack.DeletionTime = stack.DeletionTime.Format("2006-01-02T15:04:05.000Z")
	}

	return xmlStack
}

// convertToXMLStackSummary converts a Stack to XMLStackSummary.
func convertToXMLStackSummary(stack *Stack) XMLStackSummary {
	summary := XMLStackSummary{
		StackID:           stack.StackID,
		StackName:         stack.StackName,
		StackStatus:       stack.StackStatus,
		StackStatusReason: stack.StackStatusReason,
		CreationTime:      stack.CreationTime.Format("2006-01-02T15:04:05.000Z"),
	}

	if !stack.LastUpdatedTime.IsZero() {
		summary.LastUpdatedTime = stack.LastUpdatedTime.Format("2006-01-02T15:04:05.000Z")
	}

	if !stack.DeletionTime.IsZero() {
		summary.DeletionTime = stack.DeletionTime.Format("2006-01-02T15:04:05.000Z")
	}

	return summary
}

// convertToXMLStackResource converts a StackResource to XMLStackResource.
func convertToXMLStackResource(resource *StackResource) XMLStackResource {
	return XMLStackResource{
		LogicalResourceID:  resource.LogicalResourceID,
		PhysicalResourceID: resource.PhysicalResourceID,
		ResourceType:       resource.ResourceType,
		ResourceStatus:       resource.ResourceStatus,
		ResourceStatusReason: resource.Materialization,
		Timestamp:          resource.Timestamp.Format("2006-01-02T15:04:05.000Z"),
		StackID:            resource.StackID,
		StackName:          resource.StackName,
	}
}

// readCFNJSONRequest reads and decodes JSON request body.
func readCFNJSONRequest(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}

	if len(body) == 0 {
		return nil
	}

	// route66 #3923: the AWS Query dispatcher already parsed the original form
	// into r.Form and then coerced every scalar through parseFormValue, so a
	// ParameterValue of "false" or "42" reaches this decoder as a JSON bool or
	// number and the string-typed field rejects the whole request. Parameter
	// values are strings by the CloudFormation API contract, so they are rebuilt
	// from the untouched form strings rather than re-stringified from the
	// coerced JSON, which would lose spellings such as "007" or "True".
	body, err = restoreQueryStackParameters(r.Form, body)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	return nil
}

// queryParametersPrefix is the AWS Query spelling of CreateStack/UpdateStack
// Parameters list members: Parameters.member.<N>.<Field>.
const queryParametersPrefix = "Parameters.member."

// restoreQueryStackParameters replaces the dispatcher-coerced "Parameters"
// array in body with one rebuilt from the raw form strings. A request carrying
// no Parameters.member.* form keys (direct JSON callers, or a Query call with
// no parameters) is returned unchanged. Only fields actually present in the
// form are emitted, so StackParameters' missing-key/missing-value validation
// still fires on malformed input.
func restoreQueryStackParameters(form url.Values, body []byte) ([]byte, error) {
	members := map[int]map[string]string{}

	for key, values := range form {
		rest, ok := strings.CutPrefix(key, queryParametersPrefix)
		if !ok || len(values) != 1 {
			continue
		}

		idxText, field, ok := strings.Cut(rest, ".")
		if !ok || strings.Contains(field, ".") {
			return nil, fmt.Errorf("unsupported CloudFormation query parameter key %q", key)
		}

		idx, err := strconv.Atoi(idxText)
		if err != nil {
			return nil, fmt.Errorf("non-numeric member index in CloudFormation query parameter key %q", key)
		}

		if members[idx] == nil {
			members[idx] = map[string]string{}
		}

		members[idx][field] = values[0]
	}

	if len(members) == 0 {
		return body, nil
	}

	// Member order is the Query list order; StackParameters reports entries by
	// position, so the rebuilt array keeps the caller's indices sorted.
	indices := make([]int, 0, len(members))
	for idx := range members {
		indices = append(indices, idx)
	}

	sort.Ints(indices)

	entries := make([]map[string]string, 0, len(indices))
	for _, idx := range indices {
		entries = append(entries, members[idx])
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("marshal CloudFormation query parameters: %w", err)
	}

	doc["Parameters"] = raw

	return json.Marshal(doc)
}

// extractAction extracts the action name from the request.
func extractAction(r *http.Request) string {
	// Try X-Amz-Target header (format: "CloudFormation.ActionName").
	target := r.Header.Get("X-Amz-Target")
	if target != "" {
		if idx := strings.LastIndex(target, "."); idx >= 0 {
			return target[idx+1:]
		}
	}

	// Fallback to URL query parameter.
	return r.URL.Query().Get("Action")
}

// writeCFNXMLResponse writes an XML response with HTTP 200 OK.
func writeCFNXMLResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("x-amzn-RequestId", uuid.New().String())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}

// writeCFNError writes a CloudFormation error response in XML format.
func writeCFNError(w http.ResponseWriter, code, message string, status int) {
	requestID := uuid.New().String()

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("x-amzn-RequestId", requestID)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(XMLErrorResponse{
		Xmlns: cfnXMLNS,
		Error: XMLError{
			Type:    "Sender",
			Code:    code,
			Message: message,
		},
		RequestID: requestID,
	})
}

// handleCFNError handles CloudFormation errors and writes the appropriate response.
func handleCFNError(w http.ResponseWriter, err error) {
	var cfnErr *Error
	if errors.As(err, &cfnErr) {
		writeCFNError(w, cfnErr.Code, cfnErr.Message, http.StatusBadRequest)

		return
	}

	writeCFNError(w, errInternalError, "Internal server error", http.StatusInternalServerError)
}
