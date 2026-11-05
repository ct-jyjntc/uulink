//go:build ignore

package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
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
		"X-Param-user-id":   "aebgovzki4aa77yn",
		"X-Param-NONCE":     "e0597b30-c553-46df-8ad8-d8dbcceeb0f0",
		"X-Param-ENT":       "",
	}
}

func nrdSign(method, path string, params map[string]string, body []byte) string {
	pairs := make([]string, 0)
	for k := range params {
		pairs = append(pairs, "x-param-"+strings.ToLower(strings.TrimPrefix(k, "X-Param-"))+"="+params[k])
	}
	sort.Strings(pairs)
	base := method + path + strings.Join(pairs, "&") + string(body)
	mac := hmac.New(sha256.New, []byte(signSecret))
	mac.Write([]byte(base))
	return hex.EncodeToString(mac.Sum(nil))
}

func apiAppUpload(bearer string) {
	statusBody := []byte(`[{"dapp_id":"6ab224c782973fd144c1b2ec","last_launched_at":` + fmt.Sprint(time.Now().Unix()) + `,"app_status":1}]`)
	p := standardParams("aeawvostuqbdev6k", "9FA88408-C8F1-4F72-BE02-E6190912590A")
	sign := nrdSign("POST", "/api/v1/app/list/of/current_device/status", p, statusBody)
	req, _ := http.NewRequest("POST", "https://api.nrd.nie.163.com/api/v1/app/list/of/current_device/status", bytes.NewReader(statusBody))
	req.Header.Set("X-Param-SIGN", sign)
	for k, v := range p {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "UURemoteServer/624 CFNetwork/3896.200.41 Darwin/27.2.0")
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		fmt.Println(time.Now().Format("15:04:05"), "app upload err:", err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	fmt.Println(time.Now().Format("15:04:05"), "APP UPLOAD:", string(raw)[:min(120, len(raw))])
}

func main() {
	b, err := os.ReadFile("/root/uulink/bearer.txt")
	if err != nil {
		fmt.Println("bearer missing:", err)
		return
	}
	bearer := strings.TrimSpace(string(b))
	for {
		apiAppUpload(bearer)
		time.Sleep(20 * time.Second)
	}
}
