package protocol

import (
	"bufio"
	"encoding/json"
	"io"
)

type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	ID     int64           `json:"id"`
}

type Response struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ResponseError  `json:"error,omitempty"`
}

type ResponseError struct {
	Message string `json:"message"`
}

func Encode(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

func Decode(r io.Reader) (Request, error) {
	var req Request
	dec := json.NewDecoder(bufio.NewReader(r))
	if err := dec.Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

func DecodeResponse(r io.Reader) (Response, error) {
	var resp Response
	dec := json.NewDecoder(bufio.NewReader(r))
	if err := dec.Decode(&resp); err != nil {
		return resp, err
	}
	return resp, nil
}
