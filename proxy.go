package main
import (
	“bufio”
	“bytes”
	“crypto/tls”
	“encoding/json”
	“fmt”
	“io”
	“net”
	“net/http”
	“strings”
	“time”
	“github.com/andybalholm/brotli”
	“github.com/gin-gonic/gin”
	“github.com/sashabaranov/go-openai”
	log “github.com/sirupsen/logrus”
)
var (
	LLM_BASE_URL = “http://10.30.195.83:8081”
)
func HandleGinProxy(c *gin.Context) {
	HandleProxy(c.Writer, c.Request)
}
func HandleProxy(w http.ResponseWriter, r *http.Request) {
	//解析body, 根据不同的model转发到不同的模型，abab-6.5g/abab-6.5t/Moistral-11B-v3
	save, copy, _ := drainBody(r.Body)
	body, _ := io.ReadAll(save)
	var chatReq openai.ChatCompletionRequest
	json.Unmarshal(body, &chatReq)
	var baseUrl string
	var apiKey string
	if chatReq.Model == “L3-8B-Stheno-v3.2" {
		baseUrl = fmt.Sprintf(“%s/v1/chat/completions”, LLM_BASE_URL)
	} else if strings.HasPrefix(chatReq.Model, “abab”) {
		//调用minimax
		baseUrl = “https://api.minimax.chat/v1/text/chatcompletion_v2”
		apiKey = “eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJHcm91cE5hbWUiOiLlpaXluJXkupHvvIjmrabmsYnvvInnp5HmioDmnInpmZDlhazlj7giLCJVc2VyTmFtZSI6IuWlpeW4leS6ke-8iOatpuaxie-8ieenkeaKgOaciemZkOWFrOWPuCIsIkFjY291bnQiOiIiLCJTdWJqZWN0SUQiOiIxNzgxOTQzMTgzMjQ5MjQ4NzcxIiwiUGhvbmUiOiIxODYwNzE1OTY5MSIsIkdyb3VwSUQiOiIxNzgxOTQzMTgzMjQ1MDU0NDY3IiwiUGFnZU5hbWUiOiIiLCJNYWlsIjoiIiwiQ3JlYXRlVGltZSI6IjIwMjQtMDYtMTQgMTY6MDU6MzIiLCJpc3MiOiJtaW5pbWF4In0.cc1_Stc7ppeAmx29iUC2toKkWyEmTpf4lsmtqZKQ_hwHg8I-mCJfMp-1rsZ6uyMgqYZBFT53VZmKBy5-HPIk4W32uiQxTNEPfi4tFjjnqffD87Ndk7M7CRkCTr6SRDRv_jQZ6NzHCjDitQJoVS89AueMIknxlSpF4OYUbhAUZWwILVtdieoAYzlIhxSCdrPrOs9WQvLqFrpdGCTjds6Tq_2fgRgWTl649Z0Ip9oa5_103PMpsHNifofyQ01NqY41g8cIwa6EOqGku4H5jzY2kCuQ1uWetlq_U6e44p-wwsnGt3WkWUVSaxqhLuXiLa9Zx9zTJLAL78KVZINu7lrMPA”
	} else {
		log.Errorf(“model:%s is unsupported”, chatReq.Model)
		return
	}
	req, err := http.NewRequest(r.Method, baseUrl, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header = r.Header
	req.Body = copy
	//业务方可以没传header，需要补充上
	req.Header.Set(“Content-Type”, “application/json”)
	req.Header.Set(“Accept”, “*”)
	req.Header.Set(“Accept-Encoding”, “gzip, deflate, br”)
	//set api key
	if apiKey != “” {
		req.Header.Set(“Authorization”, fmt.Sprintf(“Bearer %s”, apiKey))
	}
	if chatReq.Stream {
		//流式返回
		req.Header.Set(“Transfer-Encoding”, “chunked”)
		req.Header.Set(“Connection”, “keep-alive”)
	}
	req.Header.Set(“Content-Type”, “application/json”)
	req.Header.Set(“Accept”, “*/*“)
	req.Header.Set(“Accept-Encoding”, “gzip, deflate, br”)


	rsp, err := requestLLM(req, chatReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}(rsp.Body)


	for name, values := range rsp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	head := map[string]string{
		“Cache-Control”:                    “no-store”,
		“access-control-allow-origin”:      “*”,
		“access-control-allow-credentials”: “true”,
		// “Transfer-Encoding”:                “chunked”,
		// “Connection”:                       “keep-alive”,
	}
	for k, v := range head {
		if _, ok := rsp.Header[k]; !ok {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set(“Connection”, “keep-alive”)
	contentEncoding := “”
	if chatReq.Stream {
		w.Header().Set(“Transfer-Encoding”, “chunked”)
		contentEncoding = w.Header().Get(“Content-Encoding”)
		//统一清除压缩格式
		w.Header().Del(“Content-Encoding”)
		w.Header().Del(“Content-Length”)
	}
	rsp.Header.Del(“content-security-policy”)
	rsp.Header.Del(“content-security-policy-report-only”)
	rsp.Header.Del(“clear-site-data”)
	w.WriteHeader(rsp.StatusCode)
	if chatReq.Stream {
		var scanner *bufio.Scanner
		if contentEncoding == “” {
			scanner = bufio.NewScanner(rsp.Body)
		} else if contentEncoding == “br” {
			//minimax压缩格式为br
			reader := brotli.NewReader(rsp.Body)
			scanner = bufio.NewScanner(reader)
		}
		for scanner.Scan() {
			_, _ = w.Write(scanner.Bytes())
			_, _ = w.Write([]byte(“\r\n”))
			w.(http.Flusher).Flush()
		}
		if err := scanner.Err(); err != nil {
			log.Fatalf(“failed to read response: %v”, err)
		}
	} else {
		if _, err := io.Copy(w, rsp.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}
func requestLLM(req *http.Request, chatReq openai.ChatCompletionRequest) (*http.Response, error) {
	//访问llm获取resp
	client := http.DefaultClient
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	}
	client.Transport = tr

	rsp, err := client.Do(req)

	if err != nil {
		if chatReq.Model == “L3-8B-Stheno-v3.2" {
			//重试minimax，将body里的model字段修改为abab-6.5t,打印日志+监控
			chatReq.Model = “abab-6.5t”
			baseUrl = "https://api.minimax.chat/v1/text/chatcompletion_v2"
			apiKey = “eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJHcm91cE5hbWUiOiLlpaXluJXkupHvvIjmrabmsYnvvInnp5HmioDmnInpmZDlhazlj7giLCJVc2VyTmFtZSI6IuWlpeW4leS6ke-8iOatpuaxie-8ieenkeaKgOaciemZkOWFrOWPuCIsIkFjY291bnQiOiIiLCJTdWJqZWN0SUQiOiIxNzgxOTQzMTgzMjQ5MjQ4NzcxIiwiUGhvbmUiOiIxODYwNzE1OTY5MSIsIkdyb3VwSUQiOiIxNzgxOTQzMTgzMjQ1MDU0NDY3IiwiUGFnZU5hbWUiOiIiLCJNYWlsIjoiIiwiQ3JlYXRlVGltZSI6IjIwMjQtMDYtMTQgMTY6MDU6MzIiLCJpc3MiOiJtaW5pbWF4In0.cc1_Stc7ppeAmx29iUC2toKkWyEmTpf4lsmtqZKQ_hwHg8I-mCJfMp-1rsZ6uyMgqYZBFT53VZmKBy5-HPIk4W32uiQxTNEPfi4tFjjnqffD87Ndk7M7CRkCTr6SRDRv_jQZ6NzHCjDitQJoVS89AueMIknxlSpF4OYUbhAUZWwILVtdieoAYzlIhxSCdrPrOs9WQvLqFrpdGCTjds6Tq_2fgRgWTl649Z0Ip9oa5_103PMpsHNifofyQ01NqY41g8cIwa6EOqGku4H5jzY2kCuQ1uWetlq_U6e44p-wwsnGt3WkWUVSaxqhLuXiLa9Zx9zTJLAL78KVZINu7lrMPA”
			x, _ := json.Marshal(chatReq)
			req.Body = io.NopCloser(bytes.NewReader(x))
			return requestLLM(req, chatReq)
		}
		return nil, err
	}
	return rsp, err
}
// drainBody reads all of b to memory and then returns two equivalent
// ReadClosers yielding the same bytes.
//
// It returns an error if the initial slurp of all bytes fails. It does not attempt
// to make the returned ReadClosers have identical error-matching behavior.
func drainBody(b io.ReadCloser) (r1, r2 io.ReadCloser, err error) {
	if b == nil || b == http.NoBody {
		// No copying needed. Preserve the magic sentinel meaning of NoBody.
		return http.NoBody, http.NoBody, nil
	}
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(b); err != nil {
		return nil, b, err
	}
	if err = b.Close(); err != nil {
		return nil, b, err
	}
	return io.NopCloser(&buf), io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}









