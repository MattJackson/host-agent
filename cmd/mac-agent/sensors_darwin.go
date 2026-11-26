//go:build darwin

package main

// Temperature sensors on macOS are exposed as HID services (usage page
// 0xff00, usage 5) and can be read without root through
// IOHIDEventSystemClient. This is the same route tools such as macmon use.
// It relies on IOKit symbols that Apple does not document, so if a future
// macOS release changes them, the sensors simply stop reporting (no panic).

/*
#cgo LDFLAGS: -framework CoreFoundation -framework IOKit
#include <stdio.h>
#include <stdint.h>
#include <CoreFoundation/CoreFoundation.h>

typedef struct __IOHIDEventSystemClient *IOHIDEventSystemClientRef;
typedef struct __IOHIDServiceClient *IOHIDServiceClientRef;
typedef const struct __IOHIDEvent *IOHIDEventRef;

extern IOHIDEventSystemClientRef IOHIDEventSystemClientCreate(CFAllocatorRef allocator);
extern int IOHIDEventSystemClientSetMatching(IOHIDEventSystemClientRef client, CFDictionaryRef match);
extern CFArrayRef IOHIDEventSystemClientCopyServices(IOHIDEventSystemClientRef client);
extern CFTypeRef IOHIDServiceClientCopyProperty(IOHIDServiceClientRef service, CFStringRef key);
extern IOHIDEventRef IOHIDServiceClientCopyEvent(IOHIDServiceClientRef service, int64_t type, int32_t options, int64_t timestamp);
extern double IOHIDEventGetFloatValue(IOHIDEventRef event, int32_t field);

#define kIOHIDEventTypeTemperature 15
#define kHIDPageAppleVendor 0xff00
#define kHIDUsageAppleVendorTemperature 5

// Fills names (fixed-size slots of namelen bytes) and vals with each
// temperature sensor's product name and reading in Celsius. Returns the
// number of sensors found, or -1 if the HID client could not be created.
static int hid_temps(char *names, int namelen, double *vals, int max) {
	IOHIDEventSystemClientRef client = IOHIDEventSystemClientCreate(kCFAllocatorDefault);
	if (!client) {
		return -1;
	}

	int page = kHIDPageAppleVendor;
	int usage = kHIDUsageAppleVendorTemperature;
	CFNumberRef pageNum = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &page);
	CFNumberRef usageNum = CFNumberCreate(kCFAllocatorDefault, kCFNumberIntType, &usage);
	CFMutableDictionaryRef match = CFDictionaryCreateMutable(kCFAllocatorDefault, 0,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	CFDictionarySetValue(match, CFSTR("PrimaryUsagePage"), pageNum);
	CFDictionarySetValue(match, CFSTR("PrimaryUsage"), usageNum);
	CFRelease(pageNum);
	CFRelease(usageNum);
	IOHIDEventSystemClientSetMatching(client, match);
	CFRelease(match);

	int n = 0;
	CFArrayRef services = IOHIDEventSystemClientCopyServices(client);
	if (services) {
		CFIndex count = CFArrayGetCount(services);
		for (CFIndex i = 0; i < count && n < max; i++) {
			IOHIDServiceClientRef svc = (IOHIDServiceClientRef)CFArrayGetValueAtIndex(services, i);
			IOHIDEventRef ev = IOHIDServiceClientCopyEvent(svc, kIOHIDEventTypeTemperature, 0, 0);
			if (!ev) {
				continue;
			}
			double v = IOHIDEventGetFloatValue(ev, kIOHIDEventTypeTemperature << 16);
			CFRelease(ev);

			char buf[256] = "unknown";
			CFTypeRef prod = IOHIDServiceClientCopyProperty(svc, CFSTR("Product"));
			if (prod) {
				if (CFGetTypeID(prod) == CFStringGetTypeID()) {
					CFStringGetCString((CFStringRef)prod, buf, sizeof buf, kCFStringEncodingUTF8);
				}
				CFRelease(prod);
			}
			snprintf(names + n * namelen, namelen, "%s", buf);
			vals[n] = v;
			n++;
		}
		CFRelease(services);
	}
	CFRelease(client);
	return n;
}
*/
import "C"

const maxSensors = 128

// readTemperatures returns sensor product name -> degrees Celsius.
// Sensors with no reading or an implausible value are dropped.
func readTemperatures() map[string]float64 {
	out := map[string]float64{}
	var names [maxSensors][256]C.char
	var vals [maxSensors]C.double
	n := int(C.hid_temps(&names[0][0], 256, &vals[0], maxSensors))
	for i := 0; i < n; i++ {
		v := float64(vals[i])
		if v <= -40 || v > 150 {
			continue
		}
		out[C.GoString(&names[i][0])] = v
	}
	return out
}
