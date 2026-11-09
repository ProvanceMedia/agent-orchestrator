//go:build darwin && cgo

package keychain //nolint:gocritic // cgo makes the linter see duplicate imports.

/*
#cgo LDFLAGS: -framework CoreFoundation -framework Security
#include <stdlib.h>
#include <string.h>
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>

static CFMutableDictionaryRef aoQuery(const char *service, const char *account) {
	CFMutableDictionaryRef q = CFDictionaryCreateMutable(NULL, 0,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFStringRef s = CFStringCreateWithCString(NULL, service, kCFStringEncodingUTF8);
	CFStringRef a = CFStringCreateWithCString(NULL, account, kCFStringEncodingUTF8);
	CFDictionarySetValue(q, kSecClass, kSecClassGenericPassword);
	CFDictionarySetValue(q, kSecAttrService, s);
	CFDictionarySetValue(q, kSecAttrAccount, a);
	CFRelease(s);
	CFRelease(a);
	return q;
}

static OSStatus aoGet(const char *service, const char *account, void **out, long *outLen) {
	CFMutableDictionaryRef q = aoQuery(service, account);
	CFDictionarySetValue(q, kSecReturnData, kCFBooleanTrue);
	CFDictionarySetValue(q, kSecMatchLimit, kSecMatchLimitOne);
	CFTypeRef result = NULL;
	OSStatus status = SecItemCopyMatching(q, &result);
	CFRelease(q);
	if (status != errSecSuccess) {
		return status;
	}
	CFDataRef data = (CFDataRef)result;
	CFIndex n = CFDataGetLength(data);
	void *buf = malloc(n > 0 ? n : 1);
	if (n > 0) {
		memcpy(buf, CFDataGetBytePtr(data), n);
	}
	*out = buf;
	*outLen = n;
	CFRelease(result);
	return errSecSuccess;
}

static OSStatus aoPut(const char *service, const char *account, const void *bytes, long len, const char *label) {
	CFMutableDictionaryRef q = aoQuery(service, account);
	CFDataRef data = CFDataCreate(NULL, bytes, len);
	CFMutableDictionaryRef change = CFDictionaryCreateMutable(NULL, 0,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFDictionarySetValue(change, kSecValueData, data);
	OSStatus status = SecItemUpdate(q, change);
	if (status == errSecItemNotFound) {
		CFStringRef l = CFStringCreateWithCString(NULL, label, kCFStringEncodingUTF8);
		CFDictionarySetValue(q, kSecAttrLabel, l);
		CFDictionarySetValue(q, kSecValueData, data);
		status = SecItemAdd(q, NULL);
		CFRelease(l);
	}
	CFRelease(change);
	CFRelease(data);
	CFRelease(q);
	return status;
}

static OSStatus aoDelete(const char *service, const char *account) {
	CFMutableDictionaryRef q = aoQuery(service, account);
	OSStatus status = SecItemDelete(q);
	CFRelease(q);
	return status;
}
*/
import "C" //nolint:gocritic // cgo makes the linter see duplicate imports.

import (
	"fmt"
	"unsafe" //nolint:gocritic // cgo makes the linter see duplicate imports.

	"github.com/aoagents/agent-orchestrator/backend/internal/service/connections"
)

const available = true

const (
	errSecItemNotFound          = -25300
	errSecUserCanceled          = -128
	errSecAuthFailed            = -25293
	errSecInteractionNotAllowed = -25308
)

func statusError(op string, status C.OSStatus) error {
	switch int(status) {
	case 0:
		return nil
	case errSecItemNotFound:
		return connections.ErrSecretNotFound
	case errSecUserCanceled, errSecAuthFailed, errSecInteractionNotAllowed:
		return fmt.Errorf("keychain %s: %w (OSStatus %d)", op, connections.ErrStoreDenied, int(status))
	default:
		return fmt.Errorf("keychain %s failed (OSStatus %d)", op, int(status))
	}
}

func get(service, account string) ([]byte, error) {
	cs, ca := C.CString(service), C.CString(account)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(ca))
	var out unsafe.Pointer
	var n C.long
	if err := statusError("read", C.aoGet(cs, ca, &out, &n)); err != nil { //nolint:gocritic // cgo expands this call; the source has no duplicate expression.
		return nil, err
	}
	defer C.free(out)
	return C.GoBytes(out, C.int(n)), nil
}

func put(service, account string, data []byte) error {
	cs, ca := C.CString(service), C.CString(account)
	cl := C.CString("Swarm 3.0: " + account + " connection")
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(ca))
	defer C.free(unsafe.Pointer(cl))
	var ptr unsafe.Pointer
	if len(data) > 0 {
		ptr = C.CBytes(data)
		defer C.free(ptr)
	}
	return statusError("write", C.aoPut(cs, ca, ptr, C.long(len(data)), cl))
}

func remove(service, account string) error {
	cs, ca := C.CString(service), C.CString(account)
	defer C.free(unsafe.Pointer(cs))
	defer C.free(unsafe.Pointer(ca))
	return statusError("delete", C.aoDelete(cs, ca))
}
