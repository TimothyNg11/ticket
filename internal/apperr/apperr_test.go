package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestErrorsUnwrapThroughWrapping(t *testing.T) {
	err := fmt.Errorf("context: %w", Conflict("EMAIL_TAKEN", "email already registered"))
	var ae *Error
	assert.True(t, errors.As(err, &ae))
	assert.Equal(t, http.StatusConflict, ae.Status)
	assert.Equal(t, "EMAIL_TAKEN", ae.Code)
}

func TestConstructors(t *testing.T) {
	assert.Equal(t, 400, Validation("x").Status)
	assert.Equal(t, 401, Unauthenticated("x").Status)
	assert.Equal(t, 403, Forbidden().Status)
	assert.Equal(t, 404, NotFound("event").Status)
	assert.Equal(t, "event not found", NotFound("event").Message)
	assert.Equal(t, 422, Unprocessable("C", "x").Status)
	assert.Equal(t, 503, Unavailable("x").Status)
}
