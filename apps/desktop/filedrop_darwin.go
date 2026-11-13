//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa -framework WebKit
#import <Cocoa/Cocoa.h>
#import <WebKit/WebKit.h>
#import <objc/runtime.h>

extern char *icebathDroppedFilesJS(char **paths, int count);

static NSMutableDictionary *icebathOriginalEntered = nil;

// Hands the bridge the dragged file paths before WebKit sees the drag, then runs WebKit's own handler
static NSDragOperation icebath_dragging_entered(id self, SEL _cmd, id<NSDraggingInfo> sender) {
	NSPasteboard *pb = [sender draggingPasteboard];
	NSArray *urls = [pb readObjectsForClasses:@[[NSURL class]] options:@{NSPasteboardURLReadingFileURLsOnlyKey: @YES}];
	if (urls.count > 0) {
		int count = (int)urls.count;
		char **paths = calloc(count + 1, sizeof(char *));
		for (int i = 0; i < count; i++) {
			paths[i] = strdup([[urls[i] path] UTF8String]);
		}
		char *js = icebathDroppedFilesJS(paths, count);
		for (int i = 0; i < count; i++) {
			free(paths[i]);
		}
		free(paths);
		[(WKWebView *)self evaluateJavaScript:[NSString stringWithUTF8String:js] completionHandler:nil];
		free(js);
	}
	NSValue *orig = icebathOriginalEntered[NSStringFromClass(object_getClass(self))];
	NSDragOperation (*imp)(id, SEL, id) = (NSDragOperation (*)(id, SEL, id))[orig pointerValue];
	return imp(self, _cmd, sender);
}

static void icebath_install_file_drop(void *window) {
	id win = (__bridge id)window;
	if (![win respondsToSelector:@selector(webView)]) {
		return;
	}
	WKWebView *webView = [win performSelector:@selector(webView)];
	Class cls = object_getClass(webView);
	NSString *key = NSStringFromClass(cls);
	if (icebathOriginalEntered == nil) {
		icebathOriginalEntered = [NSMutableDictionary new];
	}
	if (icebathOriginalEntered[key] != nil) {
		return;
	}
	Method m = class_getInstanceMethod(cls, @selector(draggingEntered:));
	IMP original = method_getImplementation(m);
	icebathOriginalEntered[key] = [NSValue valueWithPointer:(const void *)original];
	if (!class_addMethod(cls, @selector(draggingEntered:), (IMP)icebath_dragging_entered, method_getTypeEncoding(m))) {
		method_setImplementation(m, (IMP)icebath_dragging_entered);
	}
}
*/
import "C"

import (
	"unsafe"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//export icebathDroppedFilesJS
func icebathDroppedFilesJS(paths **C.char, count C.int) *C.char {
	list := make([]string, 0, int(count))
	for _, p := range unsafe.Slice(paths, int(count)) {
		list = append(list, C.GoString(p))
	}
	return C.CString("window.__icebathDroppedFiles && window.__icebathDroppedFiles(" + droppedFilesJSON(list) + ")")
}

// Hands the bridge each dragged file's path before the web view sees the drag
func nativeInstallFileDrop(w *application.WebviewWindow) {
	application.InvokeSync(func() {
		if p := w.NativeWindow(); p != nil {
			C.icebath_install_file_drop(p)
		}
	})
}

// NSSavePanel asks before replacing a file on its own
func nativeSaveNeedsOverwritePrompt() bool { return false }
