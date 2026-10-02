package GoTenon

import (
	"errors"
	"fmt"
)

// 预备的一些错误类型

type ErrorCode string

const (
	// ErrInactiveEffect：在非活跃上下文（UNLOADING / DISPOSED）注册 effect。
	ErrInactiveEffect ErrorCode = "INACTIVE_EFFECT"
	// ErrInvalidPlugin：插件为 nil 或类型不合法。
	ErrInvalidPlugin ErrorCode = "INVALID_PLUGIN"
	// ErrConfigInvalid：Schema 校验失败（发生在 Apply 之前）。
	ErrConfigInvalid ErrorCode = "CONFIG_INVALID"
	// ErrNotProvided：请求的服务在当前槽位没有实现。
	ErrNotProvided ErrorCode = "NOT_PROVIDED"
	// ErrDisposed：目标 Fiber 已被销毁，不可再操作。
	ErrDisposed ErrorCode = "DISPOSED"
	// ErrDuplicate：服务名或槽位已被占用。
	ErrDuplicate ErrorCode = "DUPLICATE"
	// ErrTimeout：操作超过超时预算。
	ErrTimeout ErrorCode = "TIMEOUT"
	// ErrMessageTypeMismatch：消息 Type 与 Data 实际类型不匹配(系统解析失败)。
	ErrMessageTypeMismatch ErrorCode = "MESSAGE_TYPE_MISMATCH"
	// ErrSignalUnhandled：内核收到无法处理的信号。
	ErrSignalUnhandled ErrorCode = "SIGNAL_UNHANDLED"
	// ErrMessageLoop：消息嵌套/转发超过深度上限，疑似自环。
	ErrMessageLoop ErrorCode = "MESSAGE_LOOP"
)

// CordisError 携带稳定错误码、可读信息与底层错误。
type CordisError struct {
	// Code 是稳定的机器可读分类，供 IsCode 判断。
	Code ErrorCode
	// Msg 是面向人的可读信息。
	Msg string
	// Err 是底层原因；Unwrap 后支持 errors.Is / errors.As 解链。
	Err error
}

// Error 返回可读信息：优先 Msg，其次 "CODE: 底层错误"，最后 CODE。
func (e *CordisError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return string(e.Code) + ": " + e.Err.Error()
	}
	return string(e.Code)
}

// Unwrap 返回底层错误，使 errors.Is / errors.As 可以穿透。
func (e *CordisError) Unwrap() error { return e.Err }

// IsCode 判断 err 链中是否存在指定错误码的 CordisError。
func IsCode(err error, code ErrorCode) bool {
	var ce *CordisError
	return errors.As(err, &ce) && ce.Code == code
}

// newErr 构造带格式化信息的 CordisError（包内快捷方式）。
func newErr(code ErrorCode, format string, args ...any) *CordisError {
	return &CordisError{Code: code, Msg: fmt.Sprintf(format, args...)}
}
