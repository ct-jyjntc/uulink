package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

const signSecret = "alWiSzXZTLu3WfFnw13uBru3"

func standardParams(deviceID, clientID string) map[string]string {
	return map[string]string{
		"X-Param-TS":        fmt.Sprintf("%d", time.Now().Unix()),
		"X-Param-PLAT":      "4",
		"X-Param-CHN":       "gwqd",
		"X-Param-LANG":      "zh-CN",
		"X-Param-PKGN":      "com.netease.uuremote",
		"X-Param-CNT":       "CN",
		"X-Param-OPR":       "None",
		"X-Param-VN":        "4.41.2",
		"X-Param-VC":        "624",
		"X-Param-REL":       "prod",
		"X-Param-client-id": clientID,
		"X-Param-device-id": deviceID,
		"X-Param-user-id":   runtimeConfig.UserID,
		"X-Param-NONCE":     "e0597b30-c553-46df-8ad8-d8dbcceeb0f0",
		"X-Param-ENT":       "",
	}
}

func nrdSign(method, path string, params map[string]string, body []byte) string {
	pairs := make([]string, 0)
	for k := range params {
		if k == "X-Param-SIGN" {
			continue
		}
		pairs = append(pairs, "x-param-"+strings.ToLower(strings.TrimPrefix(k, "X-Param-"))+"="+params[k])
	}
	sort.Strings(pairs)
	base := method + path + strings.Join(pairs, "&") + string(body)
	mac := hmac.New(sha256.New, []byte(signSecret))
	mac.Write([]byte(base))
	return hex.EncodeToString(mac.Sum(nil))
}
