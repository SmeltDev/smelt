package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

type keyedError struct{}

func (keyedError) Error() string     { return "x" }
func (keyedError) RequestID() string { return "abc123" }

func TestRequestIDReadsTheKeyOffTheError(t *testing.T) {
	assert.Equal(t, requestID(keyedError{}), "abc123")
	assert.Equal(t, requestID(assertErr("plain")), "")
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

type unconfirmedError struct{ keyedError }

func (unconfirmedError) ErrorCode() string { return "factory.start_unconfirmed" }

func TestJSONErrorBodyCarriesCodeAndRequestKey(t *testing.T) {
	b, err := jsonErrorBody(unconfirmedError{}, "msg", "", "")
	assert.NilError(t, err)
	var got map[string]any
	assert.NilError(t, json.Unmarshal(b, &got))
	assert.Equal(t, got["code"], "factory.start_unconfirmed")
	assert.Equal(t, got["request_id"], "abc123")

	b, err = jsonErrorBody(assertErr("plain"), "msg", "", "")
	assert.NilError(t, err)
	assert.Assert(t, !strings.Contains(string(b), "request_id"))
}
