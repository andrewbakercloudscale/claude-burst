#import <LocalAuthentication/LocalAuthentication.h>
#import <dispatch/dispatch.h>
#include <stdlib.h>
#include <string.h>
#include "tid.h"

// Compiled with -fobjc-arc (see touchid.go's CFLAGS). Two things depend on
// that and both were learned the hard way: LAContext is released rather than
// leaked once per prompt, and a __block object pointer is retained across the
// callback boundary instead of dangling.
//
// The crash this replaces: `__block NSString *msg` assigned from
// e.localizedDescription inside the reply block, then read after the
// semaphore. Without ARC that pointer is not retained, the NSError's
// autoreleased string is gone by the time the waiting thread reads it, and
// [msg UTF8String] segfaults -- taking the whole gateway down with it and
// dropping every in-flight Claude Code request. It only ever fired on the
// FAILURE path, because success passes e == nil and leaves msg as a string
// literal, which is never freed. So the happy path was proof of nothing:
// the first real Cancel crashed a process that had authenticated cleanly
// half a dozen times.
//
// Belt and braces, because the cost of being wrong here is the user's
// gateway: the message is copied to a malloc'd C string INSIDE the block,
// while the object is unambiguously still alive, so correctness no longer
// rests on __block retain semantics at all.
int tid_auth(const char *reason, char **err, int timeoutSeconds) {
    @autoreleasepool {
        LAContext *ctx = [[LAContext alloc] init];

        // LAPolicyDeviceOwnerAuthentication, not ...WithBiometrics: it
        // accepts Touch ID and falls back to the login password. Biometrics
        // alone would make this feature unavailable on a Mac with no sensor,
        // or to a user who has not enrolled a finger -- and since the caller
        // treats a failed gate as "deny", that would lock them out of their
        // own key rather than merely skipping the prompt.
        NSError *canErr = nil;
        if (![ctx canEvaluatePolicy:LAPolicyDeviceOwnerAuthentication error:&canErr]) {
            NSString *d = canErr.localizedDescription ?: @"authentication is not available on this Mac";
            *err = strdup([d UTF8String]);
            return 0;
        }

        __block int ok = 0;
        __block char *msg = NULL;
        dispatch_semaphore_t sem = dispatch_semaphore_create(0);

        [ctx evaluatePolicy:LAPolicyDeviceOwnerAuthentication
            localizedReason:[NSString stringWithUTF8String:reason]
                      reply:^(BOOL success, NSError *e) {
            ok = success ? 1 : 0;
            if (!success) {
                NSString *d = e.localizedDescription ?: @"authentication failed";
                msg = strdup([d UTF8String]);
            }
            dispatch_semaphore_signal(sem);
        }];

        if (dispatch_semaphore_wait(sem, dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeoutSeconds * NSEC_PER_SEC)) != 0) {
            // The sheet is still up and unanswered. Return rather than block
            // the HTTP handler forever. msg is deliberately not touched: the
            // block may still run and write it after this returns, and
            // reading it here would be the same lifetime bug in a new place.
            *err = strdup("timed out waiting for authentication");
            return 0;
        }

        if (!ok) {
            *err = msg ? msg : strdup("authentication failed");
            return 0;
        }
        free(msg); // NULL on success; free() of NULL is defined and a no-op.
        return 1;
    }
}
