//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type accountResponse struct {
	ID         int64  `json:"id"`
	Code       string `json:"code"`
	Type       string `json:"type"`
	Asset      string `json:"asset"`
	NormalSide int16  `json:"normal_side"`
}

func TestPostAccount_CreatesAndReturns201(t *testing.T) {
	baseURL, _ := testServer(t)
	code := "liability:customer:" + uniqueSuffix(t) + ":USDT_TRC20"

	resp := doRequest(t, http.MethodPost, baseURL+"/v1/accounts", idemKey(t), map[string]any{
		"code": code, "type": "LIABILITY", "asset": "USDT_TRC20",
	})
	assert.Equal(t, http.StatusCreated, resp.StatusCode)
	var acc accountResponse
	decodeInto(t, resp, &acc)
	assert.Equal(t, code, acc.Code)
	assert.Equal(t, "LIABILITY", acc.Type)
	assert.Equal(t, "USDT_TRC20", acc.Asset)
	assert.EqualValues(t, -1, acc.NormalSide)
}

// TestPostAccount_IdempotentOnCode covers the exact behavior C5 depends on:
// ensuring a customer's payout liability account exists is safe to call on
// every dispatch attempt, not just the first one for a given customer.
func TestPostAccount_IdempotentOnCode(t *testing.T) {
	baseURL, _ := testServer(t)
	code := "asset:tron:slot:" + uniqueSuffix(t)

	first := doRequest(t, http.MethodPost, baseURL+"/v1/accounts", idemKey(t), map[string]any{
		"code": code, "type": "ASSET", "asset": "USDT_TRC20",
	})
	require.Equal(t, http.StatusCreated, first.StatusCode)
	var firstAcc accountResponse
	decodeInto(t, first, &firstAcc)

	second := doRequest(t, http.MethodPost, baseURL+"/v1/accounts", idemKey(t), map[string]any{
		"code": code, "type": "ASSET", "asset": "USDT_TRC20",
	})
	assert.Equal(t, http.StatusCreated, second.StatusCode)
	var secondAcc accountResponse
	decodeInto(t, second, &secondAcc)
	assert.Equal(t, firstAcc.ID, secondAcc.ID)
}

func TestPostAccount_MissingIdempotencyKeyRejected(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/accounts", "", map[string]any{
		"code": "asset:tron:slot:" + uniqueSuffix(t), "type": "ASSET", "asset": "USDT_TRC20",
	})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	e := decodeError(t, resp)
	assert.Equal(t, "invalid_request", e.Error.Code)
}

func TestPostAccount_UnknownTypeRejected(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/accounts", idemKey(t), map[string]any{
		"code": "asset:tron:slot:" + uniqueSuffix(t), "type": "BOGUS", "asset": "USDT_TRC20",
	})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	e := decodeError(t, resp)
	assert.Equal(t, "invalid_request", e.Error.Code)
}

func TestPostAccount_UnknownAssetRejected(t *testing.T) {
	baseURL, _ := testServer(t)
	resp := doRequest(t, http.MethodPost, baseURL+"/v1/accounts", idemKey(t), map[string]any{
		"code": "asset:tron:slot:" + uniqueSuffix(t), "type": "ASSET", "asset": "BOGUS",
	})
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	e := decodeError(t, resp)
	assert.Equal(t, "invalid_amount", e.Error.Code)
}
