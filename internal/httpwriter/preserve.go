// Package httpwriter preserves optional net/http response-writer capabilities
// across instrumentation wrappers without advertising capabilities that the
// underlying writer does not provide.
package httpwriter

import (
	"io"
	"net/http"
)

// Unwrapper is the interface understood by http.ResponseController.
type Unwrapper interface {
	Unwrap() http.ResponseWriter
}

// CloseNotifier preserves the legacy method still required by Gin's response
// writer without making new application code depend on net/http's deprecated
// named interface. The identical method set remains assignment-compatible.
type CloseNotifier interface {
	CloseNotify() <-chan bool
}

// Features contains capability-specific delegates. A nil field means the
// wrapped writer does not provide that optional interface.
type Features struct {
	Flusher       http.Flusher
	Hijacker      http.Hijacker
	Pusher        http.Pusher
	ReaderFrom    io.ReaderFrom
	CloseNotifier CloseNotifier
}

// Preserve returns a writer with exactly the supplied optional interfaces.
// The explicit combinations are necessary because Go method sets are static;
// wrapping a dynamic http.ResponseWriter interface would otherwise hide the
// capabilities it carries.
func Preserve(base http.ResponseWriter, unwrap Unwrapper, features Features) http.ResponseWriter {
	mask := 0
	if features.Flusher != nil {
		mask |= 1
	}
	if features.Hijacker != nil {
		mask |= 2
	}
	if features.Pusher != nil {
		mask |= 4
	}
	if features.ReaderFrom != nil {
		mask |= 8
	}
	if features.CloseNotifier != nil {
		mask |= 16
	}
	switch mask {
	case 0:
		return base
	case 1:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
		}{base, unwrap, features.Flusher}
	case 2:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Hijacker
		}{base, unwrap, features.Hijacker}
	case 3:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Hijacker
		}{base, unwrap, features.Flusher, features.Hijacker}
	case 4:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Pusher
		}{base, unwrap, features.Pusher}
	case 5:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Pusher
		}{base, unwrap, features.Flusher, features.Pusher}
	case 6:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Hijacker
			http.Pusher
		}{base, unwrap, features.Hijacker, features.Pusher}
	case 7:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Hijacker
			http.Pusher
		}{base, unwrap, features.Flusher, features.Hijacker, features.Pusher}
	case 8:
		return struct {
			http.ResponseWriter
			Unwrapper
			io.ReaderFrom
		}{base, unwrap, features.ReaderFrom}
	case 9:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			io.ReaderFrom
		}{base, unwrap, features.Flusher, features.ReaderFrom}
	case 10:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Hijacker
			io.ReaderFrom
		}{base, unwrap, features.Hijacker, features.ReaderFrom}
	case 11:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Hijacker
			io.ReaderFrom
		}{base, unwrap, features.Flusher, features.Hijacker, features.ReaderFrom}
	case 12:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Pusher
			io.ReaderFrom
		}{base, unwrap, features.Pusher, features.ReaderFrom}
	case 13:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Pusher
			io.ReaderFrom
		}{base, unwrap, features.Flusher, features.Pusher, features.ReaderFrom}
	case 14:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Hijacker
			http.Pusher
			io.ReaderFrom
		}{base, unwrap, features.Hijacker, features.Pusher, features.ReaderFrom}
	case 15:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Hijacker
			http.Pusher
			io.ReaderFrom
		}{base, unwrap, features.Flusher, features.Hijacker, features.Pusher, features.ReaderFrom}
	case 16:
		return struct {
			http.ResponseWriter
			Unwrapper
			CloseNotifier
		}{base, unwrap, features.CloseNotifier}
	case 17:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			CloseNotifier
		}{base, unwrap, features.Flusher, features.CloseNotifier}
	case 18:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Hijacker
			CloseNotifier
		}{base, unwrap, features.Hijacker, features.CloseNotifier}
	case 19:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Hijacker
			CloseNotifier
		}{base, unwrap, features.Flusher, features.Hijacker, features.CloseNotifier}
	case 20:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Pusher
			CloseNotifier
		}{base, unwrap, features.Pusher, features.CloseNotifier}
	case 21:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Pusher
			CloseNotifier
		}{base, unwrap, features.Flusher, features.Pusher, features.CloseNotifier}
	case 22:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Hijacker
			http.Pusher
			CloseNotifier
		}{base, unwrap, features.Hijacker, features.Pusher, features.CloseNotifier}
	case 23:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Hijacker
			http.Pusher
			CloseNotifier
		}{base, unwrap, features.Flusher, features.Hijacker, features.Pusher, features.CloseNotifier}
	case 24:
		return struct {
			http.ResponseWriter
			Unwrapper
			io.ReaderFrom
			CloseNotifier
		}{base, unwrap, features.ReaderFrom, features.CloseNotifier}
	case 25:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			io.ReaderFrom
			CloseNotifier
		}{base, unwrap, features.Flusher, features.ReaderFrom, features.CloseNotifier}
	case 26:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Hijacker
			io.ReaderFrom
			CloseNotifier
		}{base, unwrap, features.Hijacker, features.ReaderFrom, features.CloseNotifier}
	case 27:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Hijacker
			io.ReaderFrom
			CloseNotifier
		}{base, unwrap, features.Flusher, features.Hijacker, features.ReaderFrom, features.CloseNotifier}
	case 28:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Pusher
			io.ReaderFrom
			CloseNotifier
		}{base, unwrap, features.Pusher, features.ReaderFrom, features.CloseNotifier}
	case 29:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Pusher
			io.ReaderFrom
			CloseNotifier
		}{base, unwrap, features.Flusher, features.Pusher, features.ReaderFrom, features.CloseNotifier}
	case 30:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Hijacker
			http.Pusher
			io.ReaderFrom
			CloseNotifier
		}{base, unwrap, features.Hijacker, features.Pusher, features.ReaderFrom, features.CloseNotifier}
	default:
		return struct {
			http.ResponseWriter
			Unwrapper
			http.Flusher
			http.Hijacker
			http.Pusher
			io.ReaderFrom
			CloseNotifier
		}{base, unwrap, features.Flusher, features.Hijacker, features.Pusher, features.ReaderFrom, features.CloseNotifier}
	}
}
