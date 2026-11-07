package nativehost

import "github.com/GopeedLab/gopeed/internal/appidentity"

func AllowedOrigin(origin string) bool {
	for _, id := range appidentity.ChromiumExtensionIds {
		if origin == "chrome-extension://"+id+"/" {
			return true
		}
	}
	for _, id := range appidentity.FirefoxExtensionIds {
		if origin == id {
			return true
		}
	}
	return false
}
