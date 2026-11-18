package service

import (
	"errors"
	"fmt"
	"net/http"
)

type AppError struct {
	Code       string
	Message    string
	HTTPStatus int
	Err        error
}

func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return e.Code
}

func (e *AppError) Unwrap() error { return e.Err }

func newError(code string, status int, message string, err error) *AppError {
	return &AppError{Code: code, Message: message, HTTPStatus: status, Err: err}
}

var (
	ErrInvalidArgument      = newError("INVALID_ARGUMENT", http.StatusBadRequest, "invalid argument", nil)
	ErrUnauthenticated      = newError("UNAUTHENTICATED", http.StatusUnauthorized, "missing identity", nil)
	ErrItemNotFound         = newError("ITEM_NOT_FOUND", http.StatusNotFound, "item not found", nil)
	ErrOrderNotFound        = newError("ORDER_NOT_FOUND", http.StatusNotFound, "order not found", nil)
	ErrActivityNotStarted   = newError("ACTIVITY_NOT_STARTED", http.StatusConflict, "activity has not started", nil)
	ErrActivityEnded        = newError("ACTIVITY_ENDED", http.StatusConflict, "activity has ended", nil)
	ErrSoldOut              = newError("SOLD_OUT", http.StatusConflict, "insufficient stock", nil)
	ErrLimitExceeded        = newError("LIMIT_EXCEEDED", http.StatusConflict, "purchase limit exceeded", nil)
	ErrIdempotencyConflict  = newError("IDEMPOTENCY_CONFLICT", http.StatusConflict, "idempotency key reused with different payload", nil)
	ErrResultUnknown        = newError("RESULT_UNKNOWN", http.StatusConflict, "request result is unknown", nil)
	ErrPayAfterCancel       = newError("PAY_AFTER_CANCEL", http.StatusConflict, "order already cancelled", nil)
	ErrRateLimited          = newError("RATE_LIMITED", http.StatusTooManyRequests, "rate limited", nil)
	ErrQueueFull            = newError("QUEUE_FULL", http.StatusTooManyRequests, "order queue is full", nil)
	ErrServiceUnavailable   = newError("SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "service unavailable", nil)
	ErrNotReady             = newError("NOT_READY", http.StatusServiceUnavailable, "item stock is not ready", nil)
	ErrInconsistent         = newError("SERVICE_UNAVAILABLE", http.StatusServiceUnavailable, "inconsistent durable state", nil)
	ErrPaymentAmount        = newError("INVALID_ARGUMENT", http.StatusBadRequest, "payment amount mismatch", nil)
	ErrItemNotAvailable     = newError("ACTIVITY_ENDED", http.StatusConflict, "item is not available", nil)
	ErrIdempotencyKeyNeeded = newError("INVALID_ARGUMENT", http.StatusBadRequest, "Idempotency-Key is required", nil)
)

func AsAppError(err error) (*AppError, bool) {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

func wrap(base *AppError, err error) *AppError {
	return &AppError{Code: base.Code, Message: base.Message, HTTPStatus: base.HTTPStatus, Err: err}
}
