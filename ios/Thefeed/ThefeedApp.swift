import SwiftUI
import UIKit
import WebKit

// UIKit application lifecycle (instead of the SwiftUI `App`/`WindowGroup`
// scene lifecycle, which is iOS 14+). This hosts the SwiftUI ContentView
// inside a UIHostingController so the app runs on iOS 13 as well. We
// deliberately do NOT adopt the UIScene lifecycle: a single-window app is
// simpler this way, and it keeps `UIApplication.didEnterBackground` /
// `willEnterForeground` (which ServerController observes) firing on iOS 13+.
@UIApplicationMain
final class AppDelegate: UIResponder, UIApplicationDelegate {
    var window: UIWindow?

    // Owned here so it lives for the whole app session. ContentView reaches
    // it via @EnvironmentObject; ServerController itself observes the
    // background/foreground notifications to stop/start the embedded server.
    let server = ServerController()

    func application(
        _ application: UIApplication,
        didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]?
    ) -> Bool {
        // Drop the WebView's HTTP cache (NOT localStorage) so an app update can't
        // load a STALE cached bundle from a previous version against the new
        // index.html — a version mismatch that left a blank screen. The server is
        // in-process, so refetching is instant. localStorage (settings, lang,
        // saved port) lives in a different data type and is preserved.
        let httpCacheTypes: Set<String> = [WKWebsiteDataTypeDiskCache, WKWebsiteDataTypeMemoryCache]
        WKWebsiteDataStore.default().removeData(
            ofTypes: httpCacheTypes, modifiedSince: Date(timeIntervalSince1970: 0)
        ) { /* fire-and-forget; no-store headers keep it clean afterward */ }

        let root = ContentView().environmentObject(server)
        let host = UIHostingController(rootView: root)
        let win = UIWindow(frame: UIScreen.main.bounds)
        win.rootViewController = host
        win.makeKeyAndVisible()
        window = win
        server.start()
        return true
    }
}
