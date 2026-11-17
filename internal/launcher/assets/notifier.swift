// claude-burst-notifier: posts Claude Burst's alerts to macOS Notification
// Center. It lives in its own app bundle, nested in Claude Burst.app, so
// macOS files the notifications under "Claude Burst" and asks for permission
// once, for that name. Built and signed by `claude-burst app install`
// (internal/launcher/notifier.go); run by the gateway and the support
// console (internal/alerting).
//
// Not osascript: macOS filed osascript's notifications under Script Editor
// and dropped them silently until Script Editor had been allowed to notify,
// which is why Burst stopped posting any on 3 Oct 2026.
//
//   claude-burst-notifier status
//   claude-burst-notifier request [--wait SECONDS]
//   claude-burst-notifier post --title T [--body B] [--id ID] [--group G] [--url U] [--sound] [--wait SECONDS]
//
// Each prints one JSON line: {"status": ..., "posted": ..., "error": ...}.
// status is authorized, denied, not-determined, provisional or unknown.
// Exit 0 when the command did what it was asked, 2 when macOS will not let
// it (denied, or no answer to the prompt yet), 1 on any other failure.
//
// Run with no arguments, it is macOS opening the app because a notification
// was clicked: it opens the URL the notification carries (the dashboard)
// and exits.

import AppKit
import Foundation
import UserNotifications

let center = UNUserNotificationCenter.current()

func statusName(_ s: UNAuthorizationStatus) -> String {
    switch s {
    case .authorized: return "authorized"
    case .denied: return "denied"
    case .notDetermined: return "not-determined"
    case .provisional: return "provisional"
    default: return "unknown"
    }
}

func jsonString(_ s: String) -> String {
    let data = try? JSONSerialization.data(withJSONObject: [s], options: [])
    let arr = data.flatMap { String(data: $0, encoding: .utf8) } ?? "[\"\"]"
    return String(arr.dropFirst().dropLast())
}

func reply(_ status: String, posted: Bool, error: String, code: Int32) -> Never {
    print("{\"status\":\(jsonString(status)),\"posted\":\(posted),\"error\":\(jsonString(error))}")
    exit(code)
}

// The completion handlers run on a queue of the framework's, so the main
// thread can wait on them.
func currentStatus() -> String {
    let sem = DispatchSemaphore(value: 0)
    var name = "unknown"
    center.getNotificationSettings { settings in
        name = statusName(settings.authorizationStatus)
        sem.signal()
    }
    _ = sem.wait(timeout: .now() + 10)
    return name
}

// authorize asks macOS for permission the first time and waits up to wait
// seconds for the answer; after that it only reads the answer given. The
// first alert is what asks, and it waits for the answer so that it is shown
// rather than lost.
func authorize(wait: Double) -> (String, String) {
    var st = currentStatus()
    if st != "not-determined" {
        return (st, "")
    }
    let sem = DispatchSemaphore(value: 0)
    var failure = ""
    center.requestAuthorization(options: [.alert, .sound]) { _, err in
        if let err = err { failure = err.localizedDescription }
        sem.signal()
    }
    _ = sem.wait(timeout: .now() + wait)
    st = currentStatus()
    return (st, failure)
}

final class Delegate: NSObject, UNUserNotificationCenterDelegate {
    // Shown as a banner even if macOS counts this process as the app in
    // front, which would otherwise keep it silent.
    func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .list, .sound])
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                withCompletionHandler completionHandler: @escaping () -> Void) {
        if let s = response.notification.request.content.userInfo["url"] as? String,
           let u = URL(string: s), u.scheme == "http" || u.scheme == "https" {
            NSWorkspace.shared.open(u)
        }
        completionHandler()
        DispatchQueue.main.asyncAfter(deadline: .now() + 1) { exit(0) }
    }
}

let delegate = Delegate()
center.delegate = delegate

var args = Array(CommandLine.arguments.dropFirst())
// macOS adds -psn_... to an app it launches itself on older systems.
args.removeAll { $0.hasPrefix("-psn_") }

if args.isEmpty {
    // Opened by a click on one of its notifications: the delegate gets the
    // response once the app is running, then exits. Gone after 10 seconds
    // either way, so a stray launch leaves nothing behind.
    let app = NSApplication.shared
    app.setActivationPolicy(.prohibited)
    DispatchQueue.main.asyncAfter(deadline: .now() + 10) { exit(0) }
    app.run()
    exit(0)
}

func flag(_ name: String) -> String? {
    guard let i = args.firstIndex(of: name), i + 1 < args.count else { return nil }
    return args[i + 1]
}

func waitFlag(_ def: Double) -> Double {
    if let v = flag("--wait"), let d = Double(v), d >= 0, d <= 600 { return d }
    return def
}

switch args[0] {
case "status":
    reply(currentStatus(), posted: false, error: "", code: 0)

case "request":
    let (st, err) = authorize(wait: waitFlag(120))
    reply(st, posted: false, error: err, code: (st == "authorized" || st == "provisional") ? 0 : 2)

case "post":
    guard let title = flag("--title"), !title.isEmpty else {
        reply("unknown", posted: false, error: "post needs --title", code: 1)
    }
    let (st, authErr) = authorize(wait: waitFlag(60))
    guard st == "authorized" || st == "provisional" else {
        let why = st == "denied"
            ? "Notifications for Claude Burst are turned off in System Settings > Notifications."
            : "macOS has not been answered yet: allow Claude Burst in the prompt, or in System Settings > Notifications."
        reply(st, posted: false, error: authErr.isEmpty ? why : authErr, code: 2)
    }
    let content = UNMutableNotificationContent()
    content.title = title
    content.body = flag("--body") ?? ""
    if let g = flag("--group"), !g.isEmpty { content.threadIdentifier = g }
    if let u = flag("--url"), !u.isEmpty { content.userInfo = ["url": u] }
    if args.contains("--sound") { content.sound = .default }
    let id = flag("--id") ?? UUID().uuidString
    let req = UNNotificationRequest(identifier: id, content: content, trigger: nil)
    let sem = DispatchSemaphore(value: 0)
    var failure = ""
    center.add(req) { err in
        if let err = err { failure = err.localizedDescription }
        sem.signal()
    }
    if sem.wait(timeout: .now() + 15) == .timedOut {
        reply(st, posted: false, error: "Notification Center did not answer within 15 seconds", code: 1)
    }
    reply(st, posted: failure.isEmpty, error: failure, code: failure.isEmpty ? 0 : 1)

default:
    reply("unknown", posted: false, error: "unknown command \(args[0]): status, request or post", code: 1)
}
