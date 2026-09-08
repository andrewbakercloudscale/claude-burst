// Package touchid gates an action behind the macOS device-owner
// authentication sheet -- Touch ID, falling back to the login password.
//
// It exists for exactly one caller: the admin UI's "Show" button, which
// hands a stored API key back in the clear. The gateway's OWN read of that
// key, on the failover path, must never come through here -- failover
// happens while nobody is watching, and a prompt there would turn a
// credential into a request for a fingerprint at 3am and then fail.
package touchid

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework LocalAuthentication -framework Foundation
#include <stdlib.h>
#include "tid.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

// timeoutSeconds bounds how long the sheet may sit unanswered. It is longer
// than a normal fingerprint touch because the fallback path is typing a
// login password, and shorter than any client timeout so the caller sees a
// real answer rather than a dropped connection.
const timeoutSeconds = 60

// Authenticate presents the system sheet with reason as its explanation and
// returns nil only if the device owner authenticated.
func Authenticate(reason string) error {
	cReason := C.CString(reason)
	defer C.free(unsafe.Pointer(cReason))

	var cErr *C.char
	ok := C.tid_auth(cReason, &cErr, C.int(timeoutSeconds))
	if cErr != nil {
		msg := C.GoString(cErr)
		C.free(unsafe.Pointer(cErr))
		if ok != 1 {
			return errors.New(msg)
		}
	}
	if ok != 1 {
		return errors.New("authentication failed")
	}
	return nil
}
