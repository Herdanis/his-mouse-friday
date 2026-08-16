package protocol

import (
	"bytes"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	req := Request{Method: "engage_project_agent", ID: 7}
	req.Params = []byte(`{"project":"companyA/user-service","task":"add field"}`)

	var buf bytes.Buffer
	if err := Encode(&buf, &req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := Decode(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Method != req.Method || got.ID != req.ID {
		t.Fatalf("got %+v want %+v", got, req)
	}
	if string(got.Params) != string(req.Params) {
		t.Fatalf("params: got %q want %q", got.Params, req.Params)
	}
}

func TestResponseErrorRoundTrip(t *testing.T) {
	resp := Response{ID: 7, Error: &ResponseError{Message: "daemon down"}}
	var buf bytes.Buffer
	if err := Encode(&buf, &resp); err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeResponse(&buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil || got.Error.Message != "daemon down" {
		t.Fatalf("got %+v", got)
	}
}
