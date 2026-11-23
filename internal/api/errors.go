package api

import "net/http"

// 错误码唯一来源：docs/01-algorithm-spec.md §7.4 与 api/openapi.yaml 的 Error.code 枚举。
const (
	CodeOK                  = 0
	CodeInvalidArgument     = 40001
	CodeUnauthenticated     = 40101
	CodeForbidden           = 40301
	CodeNotFound            = 40401
	CodeIdempotencyConflict = 40901
	CodeIllegalTransition   = 40902
	CodeResourceLocked      = 40903
	CodeBusinessRejected    = 42201
	CodeRateLimited         = 42901
	CodeInternal            = 50001
	CodeUnavailable         = 50301
)

// 稳定的机器可读 message，前端与测试按此匹配，不随文案调整。
const (
	msgInvalidArgument = "INVALID_ARGUMENT"
	msgNotFound        = "NOT_FOUND"
	msgInternal        = "INTERNAL_ERROR"
	msgUnavailable     = "DEPENDENCY_UNAVAILABLE"
)

// ErrorDetail 对应 openapi.yaml 的 ErrorDetail，用于指明具体字段。
type ErrorDetail struct {
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// apiError 是 handler 内部使用的错误载体，HTTP 状态码由 code 唯一推导，避免两处各写一份。
type apiError struct {
	code    int
	message string
	details []ErrorDetail
}

func errInvalidArgument(details ...ErrorDetail) *apiError {
	return &apiError{code: CodeInvalidArgument, message: msgInvalidArgument, details: details}
}

func errNotFound() *apiError {
	return &apiError{code: CodeNotFound, message: msgNotFound}
}

func errInternal() *apiError {
	return &apiError{code: CodeInternal, message: msgInternal}
}

func errUnavailable() *apiError {
	return &apiError{code: CodeUnavailable, message: msgUnavailable}
}

// errBusinessRejected 用于业务校验不通过（如策略权重非法、放置被判拒绝）。
// message 由调用方给出稳定的机器可读值。
func errBusinessRejected(message string, details ...ErrorDetail) *apiError {
	return &apiError{code: CodeBusinessRejected, message: message, details: details}
}

func httpStatus(code int) int {
	switch code {
	case CodeOK:
		return http.StatusOK
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeIdempotencyConflict, CodeIllegalTransition, CodeResourceLocked:
		return http.StatusConflict
	case CodeBusinessRejected:
		return http.StatusUnprocessableEntity
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
