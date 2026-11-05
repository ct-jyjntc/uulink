#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <ServiceManagement/ServiceManagement.h>
#import <signal.h>
#import <unistd.h>

// The app owns the UI helper and its runtime. Hiding the window keeps them alive;
// explicitly quitting the app shuts the helper down.
// Render at an explicit pixel size; the rounded tile sits inside the same
// transparent safe area at every icon resolution (100/1024 on each side).
static NSImage *LinkIcon(CGFloat size) {
    NSInteger pixels = (NSInteger)size;
    NSBitmapImageRep *rep = [[NSBitmapImageRep alloc]
        initWithBitmapDataPlanes:NULL pixelsWide:pixels pixelsHigh:pixels
        bitsPerSample:8 samplesPerPixel:4 hasAlpha:YES isPlanar:NO
        colorSpaceName:NSCalibratedRGBColorSpace bytesPerRow:pixels*4 bitsPerPixel:32];
    rep.size=NSMakeSize(size,size);
    [NSGraphicsContext saveGraphicsState];
    [NSGraphicsContext setCurrentContext:[NSGraphicsContext graphicsContextWithBitmapImageRep:rep]];
    NSAffineTransform *scale=[NSAffineTransform transform];
    [scale scaleBy:size/64.0]; [scale concat];
    NSBezierPath *tile=[NSBezierPath bezierPathWithRoundedRect:NSMakeRect(6.25,6.25,51.5,51.5) xRadius:11.5 yRadius:11.5];
    [NSGraphicsContext saveGraphicsState];
    NSShadow *shadow=[[NSShadow alloc] init]; shadow.shadowColor=[NSColor colorWithWhite:0 alpha:.20];
    shadow.shadowOffset=NSMakeSize(0,-.5);shadow.shadowBlurRadius=.75;[shadow set];
    [[NSColor colorWithSRGBRed:.30 green:.24 blue:.79 alpha:1] setFill];[tile fill];
    [NSGraphicsContext restoreGraphicsState];
    NSGradient *gradient=[[NSGradient alloc]
        initWithStartingColor:[NSColor colorWithSRGBRed:.46 green:.39 blue:.96 alpha:1]
        endingColor:[NSColor colorWithSRGBRed:.27 green:.21 blue:.76 alpha:1]];
    [gradient drawInBezierPath:tile angle:-90];
    NSBezierPath *edge=[NSBezierPath bezierPathWithRoundedRect:NSMakeRect(6.6,6.6,50.8,50.8) xRadius:11.2 yRadius:11.2];
    [[NSColor colorWithWhite:1 alpha:.18] setStroke];edge.lineWidth=.45;[edge stroke];
    NSBezierPath *arrows=[NSBezierPath bezierPath];
    [arrows moveToPoint:NSMakePoint(19,39)];[arrows lineToPoint:NSMakePoint(45,39)];
    [arrows moveToPoint:NSMakePoint(39,45)];[arrows lineToPoint:NSMakePoint(45,39)];[arrows lineToPoint:NSMakePoint(39,33)];
    [arrows moveToPoint:NSMakePoint(45,25)];[arrows lineToPoint:NSMakePoint(19,25)];
    [arrows moveToPoint:NSMakePoint(25,31)];[arrows lineToPoint:NSMakePoint(19,25)];[arrows lineToPoint:NSMakePoint(25,19)];
    arrows.lineWidth=4.5;arrows.lineCapStyle=NSLineCapStyleRound;arrows.lineJoinStyle=NSLineJoinStyleRound;
    [[NSColor whiteColor] setStroke];[arrows stroke];
    [NSGraphicsContext restoreGraphicsState];
    NSImage *image=[[NSImage alloc] initWithSize:NSMakeSize(size,size)];[image addRepresentation:rep];return image;
}

static BOOL OwnURL(NSURL *url) {
    return [url.scheme isEqualToString:@"http"] && [url.host isEqualToString:@"127.0.0.1"] && url.port.integerValue==17633;
}

@interface LinkApp : NSObject <NSApplicationDelegate, WKNavigationDelegate, WKScriptMessageHandler, WKScriptMessageHandlerWithReply, NSWindowDelegate, NSMenuItemValidation>
@property NSWindow *window;
@property id awakeActivity;
@property WKWebView *web;
@property NSTask *helper;
@property NSTimer *timer;
@property NSStatusItem *statusItem;
@property id mouseMonitor;
@property NSEvent *dragEvent;
@property dispatch_semaphore_t helperExited;
@property NSDate *readyDeadline;
@property BOOL checking;
@property BOOL loadingPage;
@property BOOL navigationFinished;
@property BOOL pageReady;
@property BOOL actionPending;
@property BOOL terminating;
@property BOOL failed;
@end

@implementation LinkApp
- (void)applicationDidFinishLaunching:(NSNotification *)note {
    // Let macOS use the multi-resolution bundled icon in Dock and Finder.
 [[NSUserDefaults standardUserDefaults] registerDefaults:@{@"UULinkKeepAwake":@NO,@"UULinkKeepInMenuBar":@YES}];
 [self updateAwake];
    NSMenu *menu = [[NSMenu alloc] init]; NSMenuItem *appItem = [[NSMenuItem alloc] init];
    [menu addItem:appItem]; NSMenu *appMenu = [[NSMenu alloc] initWithTitle:@"UU Link"];
    [appMenu addItemWithTitle:@"关于 UU Link" action:@selector(about:) keyEquivalent:@""];
    [appMenu addItem:[NSMenuItem separatorItem]];
    [appMenu addItemWithTitle:@"设置…" action:@selector(settings:) keyEquivalent:@","];
    [appMenu addItem:[NSMenuItem separatorItem]];
    [appMenu addItemWithTitle:@"隐藏 UU Link" action:@selector(hide:) keyEquivalent:@"h"];
    [appMenu addItemWithTitle:@"退出 UU Link" action:@selector(terminate:) keyEquivalent:@"q"];
    appItem.submenu=appMenu;
    NSMenuItem *editItem=[[NSMenuItem alloc] init]; [menu addItem:editItem];
    NSMenu *edit=[[NSMenu alloc] initWithTitle:@"编辑"];
    [edit addItemWithTitle:@"撤销" action:@selector(undo:) keyEquivalent:@"z"];
    [edit addItem:[NSMenuItem separatorItem]];
    [edit addItemWithTitle:@"剪切" action:@selector(cut:) keyEquivalent:@"x"];
    [edit addItemWithTitle:@"复制" action:@selector(copy:) keyEquivalent:@"c"];
    [edit addItemWithTitle:@"粘贴" action:@selector(paste:) keyEquivalent:@"v"];
    [edit addItemWithTitle:@"全选" action:@selector(selectAll:) keyEquivalent:@"a"];
    editItem.submenu=edit; [NSApp setMainMenu:menu];

    NSMenuItem *fileItem=[[NSMenuItem alloc] init];[menu insertItem:fileItem atIndex:1];
    NSMenu *fileMenu=[[NSMenu alloc] initWithTitle:@"文件"];
    NSMenuItem *closeItem=[fileMenu addItemWithTitle:@"关闭窗口" action:@selector(closeWindow:) keyEquivalent:@"w"];closeItem.target=self;fileItem.submenu=fileMenu;
    NSMenuItem *windowItem=[[NSMenuItem alloc] init];[menu addItem:windowItem];NSMenu *windowMenu=[[NSMenu alloc] initWithTitle:@"窗口"];
    NSMenuItem *minItem=[windowMenu addItemWithTitle:@"最小化" action:@selector(minimizeWindow:) keyEquivalent:@"m"];minItem.target=self;
    NSMenuItem *zoomItem=[windowMenu addItemWithTitle:@"缩放" action:@selector(zoomWindow:) keyEquivalent:@""];zoomItem.target=self;
    NSMenuItem *fullItem=[windowMenu addItemWithTitle:@"进入/退出全屏" action:@selector(fullscreenWindow:) keyEquivalent:@"f"];fullItem.keyEquivalentModifierMask=NSEventModifierFlagCommand|NSEventModifierFlagControl;fullItem.target=self;
    windowItem.submenu=windowMenu;NSApp.windowsMenu=windowMenu;

    self.statusItem=[[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
    self.statusItem.button.image=[NSImage imageWithSystemSymbolName:@"link" accessibilityDescription:@"UU Link"];
    self.statusItem.button.toolTip=@"UU Link · 关闭窗口后继续保持连接";
    NSMenu *statusMenu=[[NSMenu alloc] initWithTitle:@"UU Link"];
    for (NSArray *entry in @[@[@"打开 UU Link",NSStringFromSelector(@selector(openWindow:))],
                             @[@"连接",NSStringFromSelector(@selector(connect:))],
                             @[@"断开连接…",NSStringFromSelector(@selector(disconnect:))]]) {
        NSMenuItem *item=[statusMenu addItemWithTitle:entry[0] action:NSSelectorFromString(entry[1]) keyEquivalent:@""];
        item.target=self;
    }
    [statusMenu addItem:[NSMenuItem separatorItem]];
    NSMenuItem *quit=[statusMenu addItemWithTitle:@"退出 UU Link（断开连接）" action:@selector(terminate:) keyEquivalent:@""];
    quit.target=NSApp; self.statusItem.menu=statusMenu;

    self.window=[[NSWindow alloc] initWithContentRect:NSMakeRect(0,0,960,650)
        styleMask:NSWindowStyleMaskTitled|NSWindowStyleMaskClosable|NSWindowStyleMaskMiniaturizable|NSWindowStyleMaskResizable|NSWindowStyleMaskFullSizeContentView
        backing:NSBackingStoreBuffered defer:NO];
    self.window.title=@"UU Link"; self.window.minSize=NSMakeSize(680,470);
    self.window.titlebarAppearsTransparent=YES;
    self.window.titleVisibility=NSWindowTitleHidden;
    // AppKit's inset traffic lights share the page's 50pt header. Keep the
    // toolbar empty: page controls start after its reserved leftmost 96pt.
    NSToolbar *toolbar=[[NSToolbar alloc] initWithIdentifier:@"UULinkTitlebar"];
    toolbar.displayMode=NSToolbarDisplayModeIconOnly;
    toolbar.allowsUserCustomization=NO; toolbar.showsBaselineSeparator=NO;
    self.window.toolbarStyle=NSWindowToolbarStyleUnified;
    self.window.toolbar=toolbar; toolbar.visible=YES;
    self.window.titlebarSeparatorStyle=NSTitlebarSeparatorStyleNone;
    self.window.movableByWindowBackground=NO;
    self.window.delegate=self; self.window.releasedWhenClosed=NO;
    [self.window center];
    WKWebViewConfiguration *config=[[WKWebViewConfiguration alloc] init];
    [config.userContentController addScriptMessageHandler:self name:@"uulinkAppearance"];
 [config.userContentController addScriptMessageHandlerWithReply:self contentWorld:WKContentWorld.pageWorld name:@"uulinkPreferences"];
    [config.userContentController addScriptMessageHandler:self name:@"uulinkExport"];
    [config.userContentController addScriptMessageHandler:self name:@"uulinkDrag"];
    self.web=[[WKWebView alloc] initWithFrame:self.window.contentView.bounds configuration:config];
    self.web.autoresizingMask=NSViewWidthSizable|NSViewHeightSizable;
    self.web.navigationDelegate=self; self.window.contentView=self.web;
    // Retain the actual mouse-down event. WKScriptMessage is asynchronous, so
    // NSApp.currentEvent is not necessarily the event that began the gesture.
    __weak LinkApp *weakSelf=self;
    self.mouseMonitor=[NSEvent addLocalMonitorForEventsMatchingMask:NSEventMaskLeftMouseDown|NSEventMaskLeftMouseUp handler:^NSEvent *(NSEvent *event) {
        LinkApp *app=weakSelf; app.dragEvent=nil;
        if (event.type!=NSEventTypeLeftMouseDown || event.window!=app.window || app.window.attachedSheet || !app.pageReady) return event;
        NSPoint p=[app.web convertPoint:event.locationInWindow fromView:nil];
        CGFloat top=app.web.isFlipped ? p.y : NSHeight(app.web.bounds)-p.y;
        if (top<0 || top>=50 || !NSPointInRect(p,app.web.bounds)) return event;
        for (NSNumber *kind in @[@(NSWindowCloseButton),@(NSWindowMiniaturizeButton),@(NSWindowZoomButton)]) {
            NSButton *button=[app.window standardWindowButton:kind.integerValue];
            if (button && NSPointInRect(p,[app.web convertRect:button.bounds fromView:button])) return event;
        }
        app.dragEvent=event;
        return event; // Never consume a page control's mouse-down.
    }];
    [self.window makeKeyAndOrderFront:nil]; [NSApp activateIgnoringOtherApps:YES];

    self.helper=[[NSTask alloc] init];
    self.helper.executableURL=[[NSBundle mainBundle] URLForResource:@"uulink-ui" withExtension:nil];
    if (!self.helper.executableURL) { [self fail:@"应用包缺少 uulink-ui 运行程序，请重新安装完整的 UU Link.app。"]; return; }
    self.helper.arguments=@[@"gui",@"--no-open",@"--parent-pid",[NSString stringWithFormat:@"%d",getpid()]];
    // Unread pipes can fill up and stall the child. Do not expose runtime logs
    // or authentication data in a native alert.
    self.helper.standardOutput=[NSFileHandle fileHandleWithNullDevice];
    self.helper.standardError=[NSFileHandle fileHandleWithNullDevice];
    self.helperExited=dispatch_semaphore_create(0);
    dispatch_semaphore_t exited=self.helperExited;
    self.helper.terminationHandler=^(NSTask *task) {
        dispatch_semaphore_signal(exited);
        dispatch_async(dispatch_get_main_queue(),^{
            LinkApp *app=weakSelf;
            if (!app || app.terminating || app.failed) return;
            NSString *text=app.pageReady ? @"连接运行程序意外退出，当前页面已停止使用。请重新打开 UU Link。" : @"本次运行程序在界面就绪前退出。可能已有旧实例占用端口，或运行程序与应用版本不匹配。请关闭旧实例后重新打开 UU Link。";
            [app fail:[text stringByAppendingFormat:@"\n退出状态：%d",task.terminationStatus]];
        });
    };
    NSError *error=nil;
    if (![self.helper launchAndReturnError:&error]) { [self fail:[@"无法启动连接运行程序：" stringByAppendingString:error.localizedDescription ?: @"未知错误"]]; return; }
    self.readyDeadline=[NSDate dateWithTimeIntervalSinceNow:15];
    self.timer=[NSTimer scheduledTimerWithTimeInterval:.15 target:self selector:@selector(ready:) userInfo:nil repeats:YES];
}
- (void)ready:(NSTimer *)timer {
    if (self.terminating || self.failed) return;
    if (self.readyDeadline.timeIntervalSinceNow<=0) {
        [self fail:self.loadingPage ? @"页面或本机认证状态加载超时。请重新打开 UU Link；若问题持续，请检查应用包是否完整。" : @"本次连接运行程序未能在 15 秒内就绪。请检查是否有旧实例或其他程序占用本机 17633 端口。"]; return;
    }
    if (!self.helper.running) return; // terminationHandler reports the failure.
    if(self.checking) return;
    if (self.loadingPage) {
        if (!self.navigationFinished || !OwnURL(self.web.URL)) return;
        self.checking=YES;
        [self.web evaluateJavaScript:@"(typeof window.uulinkMenuAction === 'function' || typeof serviceAction === 'function') && typeof state !== 'undefined' && state !== null && !!document.querySelector('#main')" completionHandler:^(id value,NSError *error) {
            self.checking=NO;
            if (self.failed || self.terminating || !self.helper.running || !self.navigationFinished || !OwnURL(self.web.URL)) return;
            if (!error && [value isEqual:@YES]) {
                self.pageReady=YES; [self.timer invalidate]; self.timer=nil;
            }
        }];
        return;
    }
    NSString *path=[NSHomeDirectory() stringByAppendingPathComponent:@".config/uulink-ui/session.json"];
    NSData *data=[NSData dataWithContentsOfFile:path];
    id decoded=data ? [NSJSONSerialization JSONObjectWithData:data options:0 error:nil] : nil;
    NSDictionary *record=[decoded isKindOfClass:NSDictionary.class] ? decoded : nil;
    NSString *link=[record[@"URL"] isKindOfClass:NSString.class] ? record[@"URL"] : nil;
    NSURL *url=link ? [NSURL URLWithString:link] : nil;
    // Never attach this app to an older helper merely because it answers HTTP.
    NSNumber *pid=[record[@"PID"] isKindOfClass:NSNumber.class] ? record[@"PID"] : nil;
    if (OwnURL(url) && pid.intValue==self.helper.processIdentifier) {
        NSString *key=[record[@"Key"] isKindOfClass:NSString.class] ? record[@"Key"] : nil;
        if(key.length==64) {
            self.checking=YES;
            NSMutableURLRequest *req=[NSMutableURLRequest requestWithURL:[NSURL URLWithString:@"http://127.0.0.1:17633/api/health"]];
            req.timeoutInterval=.7; [req setValue:key forHTTPHeaderField:@"X-UULink-Key"];
            [[[NSURLSession sharedSession] dataTaskWithRequest:req completionHandler:^(NSData *body,NSURLResponse *resp,NSError *error){
                dispatch_async(dispatch_get_main_queue(),^{
                    self.checking=NO;
                    if(!self.timer || self.failed || self.terminating || !self.helper.running) return;
                    if(!error && OwnURL(resp.URL) && [resp isKindOfClass:NSHTTPURLResponse.class] && ((NSHTTPURLResponse *)resp).statusCode==200) {
                        self.loadingPage=YES; self.readyDeadline=[NSDate dateWithTimeIntervalSinceNow:15];
                        [self.web loadRequest:[NSURLRequest requestWithURL:url]];
                    }
                });
            }] resume];
        }
    }
}
- (void)webView:(WKWebView *)webView decidePolicyForNavigationAction:(WKNavigationAction *)action decisionHandler:(void (^)(WKNavigationActionPolicy))decisionHandler {
    NSURL *u=action.request.URL;
    decisionHandler(OwnURL(u) && !self.failed && !self.terminating ? WKNavigationActionPolicyAllow : WKNavigationActionPolicyCancel);
}
- (void)webView:(WKWebView *)webView didStartProvisionalNavigation:(WKNavigation *)navigation {
    self.pageReady=NO; self.navigationFinished=NO; self.loadingPage=YES; self.dragEvent=nil;
    self.readyDeadline=[NSDate dateWithTimeIntervalSinceNow:15];
    if (!self.timer && !self.failed && !self.terminating) self.timer=[NSTimer scheduledTimerWithTimeInterval:.15 target:self selector:@selector(ready:) userInfo:nil repeats:YES];
}
- (void)webView:(WKWebView *)webView didFinishNavigation:(WKNavigation *)navigation { self.navigationFinished=YES; }
- (void)webView:(WKWebView *)webView didFailProvisionalNavigation:(WKNavigation *)navigation withError:(NSError *)error {
    if (error.code!=NSURLErrorCancelled) [self fail:@"无法加载本机页面。连接运行程序可能不可用，请重新打开 UU Link。"];
}
- (void)webView:(WKWebView *)webView didFailNavigation:(WKNavigation *)navigation withError:(NSError *)error {
    if (error.code!=NSURLErrorCancelled) [self fail:@"本机页面加载中断，请重新打开 UU Link。"];
}
- (void)webViewWebContentProcessDidTerminate:(WKWebView *)webView { [self fail:@"界面进程意外退出，请重新打开 UU Link。"]; }
- (void)updateAwake {
 BOOL enabled=[[NSUserDefaults standardUserDefaults] boolForKey:@"UULinkKeepAwake"];
 if(enabled && !self.awakeActivity) self.awakeActivity=[[NSProcessInfo processInfo] beginActivityWithOptions:NSActivityIdleSystemSleepDisabled reason:@"UU Link 保持连接"];
 if(!enabled && self.awakeActivity){[[NSProcessInfo processInfo] endActivity:self.awakeActivity];self.awakeActivity=nil;}
}
- (NSDictionary *)preferences {
 SMAppServiceStatus status=SMAppService.mainAppService.status;
 return @{@"login":@(status==SMAppServiceStatusEnabled),@"loginApproval":@(status==SMAppServiceStatusRequiresApproval),@"awake":@([[NSUserDefaults standardUserDefaults] boolForKey:@"UULinkKeepAwake"]),@"menuBar":@([[NSUserDefaults standardUserDefaults] boolForKey:@"UULinkKeepInMenuBar"]),@"dock":@([[NSUserDefaults standardUserDefaults] boolForKey:@"UULinkShowInDock"])};
}
- (void)userContentController:(WKUserContentController *)controller didReceiveScriptMessage:(WKScriptMessage *)message replyHandler:(void (^)(id _Nullable,NSString * _Nullable))replyHandler {
 if(self.failed || self.terminating || !message.frameInfo.isMainFrame || !OwnURL(message.frameInfo.request.URL) || ![message.body isKindOfClass:NSDictionary.class]){replyHandler(nil,@"应用状态不可用");return;}
 NSDictionary *body=message.body;NSString *action=body[@"action"];
 if([action isEqual:@"get"]){replyHandler([self preferences],nil);return;}
 if([action isEqual:@"loginSettings"]){[SMAppService openSystemSettingsLoginItems];replyHandler([self preferences],nil);return;}
 if(![action isEqual:@"set"] || ![body[@"value"] isKindOfClass:NSNumber.class]){replyHandler(nil,@"无效设置");return;}
 NSString *key=body[@"key"];BOOL value=[body[@"value"] boolValue];
 if([key isEqual:@"login"]){
  SMAppService *service=SMAppService.mainAppService;NSError *error=nil;BOOL ok=YES;
  if(value && service.status!=SMAppServiceStatusEnabled && service.status!=SMAppServiceStatusRequiresApproval)ok=[service registerAndReturnError:&error];
  if(!value && service.status!=SMAppServiceStatusNotRegistered)ok=[service unregisterAndReturnError:&error];
  if(!ok){replyHandler(nil,[@"登录自启设置失败：" stringByAppendingString:error.localizedDescription ?: @"请检查系统登录项"]);return;}
 }else if([key isEqual:@"awake"]){[[NSUserDefaults standardUserDefaults] setBool:value forKey:@"UULinkKeepAwake"];[self updateAwake];}
 else if([key isEqual:@"dock"]){
  if(![NSApp setActivationPolicy:value ? NSApplicationActivationPolicyRegular : NSApplicationActivationPolicyAccessory]){replyHandler(nil,@"无法更改 Dock 图标显示");return;}
  [[NSUserDefaults standardUserDefaults] setBool:value forKey:@"UULinkShowInDock"];
  if(self.window.isVisible){[self.window makeKeyAndOrderFront:nil];[NSApp activateIgnoringOtherApps:YES];}
 }
 else if([key isEqual:@"menuBar"]){[[NSUserDefaults standardUserDefaults] setBool:value forKey:@"UULinkKeepInMenuBar"];}
 else{replyHandler(nil,@"无效设置");return;}
 replyHandler([self preferences],nil);
}
- (void)userContentController:(WKUserContentController *)controller didReceiveScriptMessage:(WKScriptMessage *)message {
    NSURL *origin=message.frameInfo.request.URL;
    if(self.failed || self.terminating || !message.frameInfo.isMainFrame || !OwnURL(origin)) return;
    if ([message.name isEqualToString:@"uulinkAppearance"] && [message.body isKindOfClass:NSString.class]) {
        NSString *name=[message.body isEqualToString:@"dark"] ? NSAppearanceNameDarkAqua : NSAppearanceNameAqua;
        self.window.appearance=[message.body isEqualToString:@"system"] ? nil : [NSAppearance appearanceNamed:name];
    }
    if ([message.name isEqualToString:@"uulinkExport"] && [message.body isKindOfClass:NSString.class]) {
        NSString *text=message.body;
        if(text.length>300000) return;
        NSSavePanel *panel=[NSSavePanel savePanel]; panel.nameFieldStringValue=@"uulink.log";
        [panel beginSheetModalForWindow:self.window completionHandler:^(NSModalResponse response){
            if(response==NSModalResponseOK){NSError *error=nil;[text writeToURL:panel.URL atomically:YES encoding:NSUTF8StringEncoding error:&error];if(error){NSAlert *alert=[NSAlert alertWithError:error];[alert beginSheetModalForWindow:self.window completionHandler:nil];}}
        }];
    }
    if([message.name isEqualToString:@"uulinkDrag"] && [message.body isEqual:@"drag"]) {
        NSEvent *event=self.dragEvent; self.dragEvent=nil;
        if (self.pageReady && !self.window.attachedSheet && event.window==self.window && (NSEvent.pressedMouseButtons&1) && NSProcessInfo.processInfo.systemUptime-event.timestamp<1) {
            [self.window performWindowDragWithEvent:event];
        }
    }
}
- (void)closeWindow:(id)sender { [self.window performClose:sender]; }
- (void)minimizeWindow:(id)sender { [self.window performMiniaturize:sender]; }
- (void)zoomWindow:(id)sender { [self.window performZoom:sender]; }
- (void)fullscreenWindow:(id)sender { [self.window toggleFullScreen:sender]; }
- (void)openWindow:(id)sender {
    if (self.terminating) return;
    if (self.window.miniaturized) [self.window deminiaturize:nil];
    [self.window makeKeyAndOrderFront:nil]; [NSApp activateIgnoringOtherApps:YES];
}
- (BOOL)validateMenuItem:(NSMenuItem *)item {
    if (item.action==@selector(connect:) || item.action==@selector(disconnect:) || item.action==@selector(settings:))
        return self.pageReady && self.helper.running && OwnURL(self.web.URL) && !self.actionPending && !self.failed && !self.terminating && !self.window.attachedSheet;
    return !self.terminating;
}
- (void)pageCommand:(NSString *)command {
    [self openWindow:nil];
    if (!self.pageReady || !self.helper.running || !OwnURL(self.web.URL) || self.actionPending || self.failed || self.terminating || self.window.attachedSheet) { NSBeep(); return; }
    self.actionPending=YES;
    NSString *script=@"if (location.origin !== 'http://127.0.0.1:17633' || typeof state === 'undefined' || state === null) return 'not-ready';"
        "if (document.querySelector('dialog[open], .modal:not([hidden])')) return 'modal';"
        "if (command === 'settings') { if (typeof setView !== 'function') return 'not-ready'; setView('settings'); }"
        "else if (typeof window.uulinkMenuAction === 'function') { await window.uulinkMenuAction(command === 'start' ? 'connect' : 'disconnect'); }"
        "else if (typeof serviceAction === 'function') { await serviceAction(command); } else return 'not-ready'; return 'ok';";
    [self.web callAsyncJavaScript:script arguments:@{@"command":command} inFrame:nil inContentWorld:WKContentWorld.pageWorld completionHandler:^(id value,NSError *error) {
        self.actionPending=NO;
        if (self.failed || self.terminating) return;
        if (error || ![value isEqual:@"ok"]) {
            NSAlert *alert=[[NSAlert alloc] init]; alert.messageText=@"暂时无法执行操作";
            alert.informativeText=[value isEqual:@"modal"] ? @"请先完成或关闭当前对话框，再使用菜单栏操作。" : @"本机页面尚未就绪或操作未能完成，请在窗口中检查连接状态后重试。";
            if (!self.window.attachedSheet) [alert beginSheetModalForWindow:self.window completionHandler:nil];
        }
    }];
}
- (void)connect:(id)sender { [self pageCommand:@"start"]; }
- (void)disconnect:(id)sender { [self pageCommand:@"stop"]; }
- (void)settings:(id)sender { [self pageCommand:@"settings"]; }
- (void)about:(id)sender {
    NSAlert *a=[[NSAlert alloc] init];a.messageText=@"UU Link";
    [self openWindow:nil];
    if (self.window.attachedSheet) return;
    NSDictionary *info=NSBundle.mainBundle.infoDictionary;
    a.informativeText=[NSString stringWithFormat:@"%@ (%@)\nTCP + UDP 端口映射",info[@"CFBundleShortVersionString"] ?: @"",info[@"CFBundleVersion"] ?: @""];
    a.icon=LinkIcon(128); [a beginSheetModalForWindow:self.window completionHandler:nil];
}
- (void)fail:(NSString *)text {
    if (self.failed || self.terminating) return;
    self.failed=YES; self.pageReady=NO; self.dragEvent=nil;
    [self.timer invalidate]; self.timer=nil; [self.web stopLoading]; self.web.hidden=YES;
    [self openWindow:nil];
    if (self.window.attachedSheet) [self.window endSheet:self.window.attachedSheet returnCode:NSModalResponseCancel];
    NSAlert *a=[[NSAlert alloc] init];a.messageText=@"UU Link 无法继续运行";a.informativeText=text;
    [a runModal]; [NSApp terminate:nil];
}
- (BOOL)windowShouldClose:(NSWindow *)sender { if ([[NSUserDefaults standardUserDefaults] boolForKey:@"UULinkKeepInMenuBar"]) [sender orderOut:nil]; else [NSApp terminate:nil]; return NO; }
- (BOOL)applicationShouldTerminateAfterLastWindowClosed:(NSApplication *)sender { return NO; }
- (BOOL)applicationShouldHandleReopen:(NSApplication *)sender hasVisibleWindows:(BOOL)visible { [self openWindow:nil]; return YES; }
- (NSApplicationTerminateReply)applicationShouldTerminate:(NSApplication *)sender {
    if (self.terminating) return NSTerminateLater;
    self.terminating=YES; self.pageReady=NO; self.dragEvent=nil;
    [self.timer invalidate]; self.timer=nil; [self.web stopLoading];
    if (self.mouseMonitor) { [NSEvent removeMonitor:self.mouseMonitor]; self.mouseMonitor=nil; }
    NSTask *task=self.helper;
    if (!task.running) return NSTerminateNow;
    // Give the owned runtime four seconds to clean up, then reserve one second
    // for forced exit. Never wait on the main thread or on an unrelated PID.
    dispatch_semaphore_t exited=self.helperExited;
    dispatch_time_t gracefulDeadline=dispatch_time(DISPATCH_TIME_NOW,4*NSEC_PER_SEC);
    dispatch_time_t exitDeadline=dispatch_time(DISPATCH_TIME_NOW,5*NSEC_PER_SEC);
    [task terminate];
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_USER_INITIATED,0),^{
        if (dispatch_semaphore_wait(exited,gracefulDeadline)!=0 && task.running) {
            kill(task.processIdentifier,SIGKILL);
            dispatch_semaphore_wait(exited,exitDeadline);
        }
        dispatch_async(dispatch_get_main_queue(),^{ [NSApp replyToApplicationShouldTerminate:YES]; });
    });
    return NSTerminateLater;
}
- (void)applicationWillTerminate:(NSNotification *)note {
 if(self.awakeActivity){[[NSProcessInfo processInfo] endActivity:self.awakeActivity];self.awakeActivity=nil;}
    [self.timer invalidate];
    if (self.mouseMonitor) [NSEvent removeMonitor:self.mouseMonitor];
    if (self.statusItem) [[NSStatusBar systemStatusBar] removeStatusItem:self.statusItem];
}
@end

int main(int argc,const char *argv[]) {
    @autoreleasepool {
        if (argc==3 && strcmp(argv[1],"--render-iconset")==0) {
            NSString *dir=[NSString stringWithUTF8String:argv[2]];
            [[NSFileManager defaultManager] createDirectoryAtPath:dir withIntermediateDirectories:YES attributes:nil error:nil];
            for (int size=16;size<=512;size*=2) {
                for (int scale=1;scale<=2;scale++) {
                    NSImage *icon=LinkIcon(size*scale);
                    NSBitmapImageRep *rep=[NSBitmapImageRep imageRepWithData:icon.TIFFRepresentation];
                    NSData *png=[rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
                    NSString *name=[NSString stringWithFormat:@"icon_%dx%d%@.png",size,size,scale==2?@"@2x":@""];
                    [png writeToFile:[dir stringByAppendingPathComponent:name] atomically:YES];
                }
            }
            return 0;
        }
        NSApplication *app=[NSApplication sharedApplication];
        [[NSUserDefaults standardUserDefaults] registerDefaults:@{@"UULinkShowInDock":@YES}];
        [app setActivationPolicy:[[NSUserDefaults standardUserDefaults] boolForKey:@"UULinkShowInDock"] ? NSApplicationActivationPolicyRegular : NSApplicationActivationPolicyAccessory];
        LinkApp *delegate=[[LinkApp alloc] init];app.delegate=delegate;[app run];
    }
    return 0;
}
