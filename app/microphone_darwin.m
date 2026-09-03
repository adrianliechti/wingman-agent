#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>

// go-shell owns the WKWebView and its private ShellApp delegate. This category
// supplies the one optional delegate method Wingman needs without widening the
// shell package's public API. Only the app's main-frame loopback origin may use
// the microphone; camera and embedded/external origins remain denied.
@interface ShellApp : NSObject
@end

@implementation ShellApp (WingmanMediaCapture)

- (void)webView:(WKWebView *)webView
    requestMediaCapturePermissionForOrigin:(WKSecurityOrigin *)origin
                          initiatedByFrame:(WKFrameInfo *)frame
                                      type:(WKMediaCaptureType)type
                           decisionHandler:(void (^)(WKPermissionDecision decision))decisionHandler
    API_AVAILABLE(macos(12.0)) {
    NSURL *url = webView.URL;
    BOOL sameOrigin = url != nil &&
        [origin.protocol isEqualToString:url.scheme] &&
        [origin.host isEqualToString:url.host] &&
        url.port != nil && origin.port == url.port.integerValue;
    BOOL microphoneOnly = type == WKMediaCaptureTypeMicrophone;

    decisionHandler(
        sameOrigin && frame.mainFrame && microphoneOnly
            ? WKPermissionDecisionGrant
            : WKPermissionDecisionDeny
    );
}

@end

