#ifndef CLAUDE_BURST_TID_H
#define CLAUDE_BURST_TID_H

// tid_auth presents the system authentication sheet (Touch ID, with the
// login password as the fallback) and blocks until the user answers.
//
// Returns 1 when the device owner authenticated, 0 otherwise. On failure
// *err receives a malloc'd, caller-freed message. timeoutSeconds bounds the
// wait so an unanswered sheet cannot pin an HTTP handler open forever.
int tid_auth(const char *reason, char **err, int timeoutSeconds);

#endif
