package main

import (
	// 标准库
	"crypto/tls"
	"bufio"
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	handleUrl "net/url"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"sort"

	// 本地包
	"MediaProxy/base"

	// 第三方库
	"github.com/bzsome/chaoGo/workpool"
	"github.com/go-resty/resty/v2"
	"github.com/patrickmn/go-cache"
	"github.com/sirupsen/logrus"
	"github.com/miekg/dns"
)

//go:embed static/index.html
//go:embed config.json
var embedRes embed.FS

var workPool = false
var proxyTimeout = int64(10)
var mediaCache = cache.New(4*time.Hour, 10*time.Minute)

type SSLConfig struct {
    Cert *string `json:"cert"`
    Key  *string `json:"key"`
}

type Config struct {
	WorkPool *bool           `json:"workPool"`
	Debug    *bool           `json:"debug"`
	Port     json.RawMessage `json:"port"`
	SSL      *SSLConfig      `json:"ssl"`
	DNS      *string         `json:"dns"`
}

type Chunk struct {
	startOffset int64
	endOffset   int64
	buffer      []byte
}

func newChunk(start int64, end int64) *Chunk {
	return &Chunk{
		startOffset: start,
		endOffset:   end,
	}
}

func (ch *Chunk) get() []byte {
	return ch.buffer
}

func (ch *Chunk) put(buffer []byte) {
	ch.buffer = buffer
}

type ProxyDownloadStruct struct {
	ProxyRunning         bool
	NextChunkStartOffset int64
	CurrentOffset        int64
	CurrentChunk         int64
	ChunkSize            int64
	MaxBufferedChunk     int64
	startOffset          int64
	EndOffset            int64
	ProxyMutex           *sync.Mutex
	ProxyTimeout         int64
	ReadyChunkQueue      chan *Chunk
	ThreadCount          int64
	DownloadUrl          string
	CookieJar            *cookiejar.Jar
	OriginThreadNum      int
}

func newProxyDownloadStruct(downloadUrl string, proxyTimeout int64, maxBuferredChunk int64, chunkSize int64, startOffset int64, endOffset int64, numTasks int64, cookiejar *cookiejar.Jar, originThreadNum int) *ProxyDownloadStruct {
	return &ProxyDownloadStruct{
		ProxyRunning:         true,
		MaxBufferedChunk:     int64(maxBuferredChunk),
		ProxyTimeout:         proxyTimeout,
		ReadyChunkQueue:      make(chan *Chunk, maxBuferredChunk),
		ProxyMutex:           &sync.Mutex{},
		ChunkSize:            chunkSize,
		NextChunkStartOffset: startOffset,
		CurrentOffset:        startOffset,
		startOffset:          startOffset,
		EndOffset:            endOffset,
		ThreadCount:          numTasks,
		DownloadUrl:          downloadUrl,
		CookieJar:            cookiejar,
		OriginThreadNum:      originThreadNum,
	}
}

func ConcurrentDownload(p *ProxyDownloadStruct, downloadUrl string, rangeStart int64, rangeEnd int64, splitSize int64, numTasks int64, emitter *base.Emitter, req *http.Request, jar *cookiejar.Jar) {
	totalLength := rangeEnd - rangeStart + 1
	numSplits := int64(math.Ceil(float64(totalLength) / float64(splitSize)))
	if numSplits > int64(numTasks) {
		numSplits = int64(numTasks)
	}

	logrus.Debugf("正在处理: %+v, rangeStart: %+v, rangeEnd: %+v, contentLength :%+v, splitSize: %+v, numSplits: %+v, numTasks: %+v", downloadUrl, rangeStart, rangeEnd, totalLength, splitSize, numSplits, numTasks)

	if workPool {
		var wp *workpool.WorkPool
		workPoolKey := downloadUrl + "#Workpool"
		if x, found := mediaCache.Get(workPoolKey); found {
			wp = x.(*workpool.WorkPool)
			if ! workPool {
				wp = workpool.New(int(numTasks))
				wp.SetTimeout(time.Duration(proxyTimeout) * time.Second)
				mediaCache.Set(workPoolKey, wp, 14400*time.Second)
			}
		} else {
			wp = workpool.New(int(numTasks))
			wp.SetTimeout(time.Duration(proxyTimeout) * time.Second)
			mediaCache.Set(workPoolKey, wp, 14400*time.Second)
		}
		for numSplit := 0; numSplit < int(numSplits); numSplit++ {
			wp.Do(func() error {
				p.ProxyWorker(req)
				return nil
			})
		}
	} else {
		for numSplit := 0; numSplit < int(numSplits); numSplit++ {
			go p.ProxyWorker(req)
		}
	}

	defer func() {
		p.ProxyStop()
		p = nil
	}()

	for {
		buffer := p.ProxyRead()

		if len(buffer) == 0 {
			p.ProxyStop()
			emitter.Close()
			logrus.Debugf("ProxyRead执行失败")
			buffer = nil
			return
		}

		_, err := emitter.Write(buffer)

		if err != nil {
			p.ProxyStop()
			emitter.Close()
			logrus.Errorf("emitter写入失败, 错误: %+v", err)
			buffer = nil
			return
		}

		if p.CurrentOffset >= rangeEnd {
			p.ProxyStop()
			emitter.Close()
			logrus.Debugf("所有服务已经完成大小: %+v", totalLength)
			buffer = nil
			return
		}
		buffer = nil
	}
}

func (p *ProxyDownloadStruct) ProxyRead() []byte {
	// 判断文件是否下载结束
	if p.CurrentOffset >= p.EndOffset {
		p.ProxyStop()
		return nil
	}

	// 获取当前的chunk的数据
	var currentChunk *Chunk
	select {
	case currentChunk = <-p.ReadyChunkQueue:
		break
	case <-time.After(time.Duration(p.ProxyTimeout) * time.Second):
		logrus.Debugf("执行 ProxyRead 超时")
		p.ProxyStop()
		return nil
	}

	for {
		if !p.ProxyRunning {
			break
		}
		buffer := currentChunk.get()
		if len(buffer) > 0 {
			p.CurrentOffset += int64(len(buffer))
			currentChunk = nil
			return buffer
		} else {
			time.Sleep(50 * time.Millisecond)
		}
	}
	currentChunk = nil
	return nil
}

func (p *ProxyDownloadStruct) ProxyStop() {
	p.ProxyRunning = false
	var currentChunk *Chunk
	for {
		select {
		case currentChunk = <-p.ReadyChunkQueue:
			currentChunk.buffer = nil
			currentChunk = nil
		case <-time.After(1 * time.Second):
			return
		}
	}
}

func (p *ProxyDownloadStruct) ProxyWorker(req *http.Request) {
	logrus.Debugf("当前活跃的协程数量: %d", runtime.NumGoroutine()-p.OriginThreadNum)
	for {
		if !p.ProxyRunning {
			break
		}

		p.ProxyMutex.Lock()
		// 生成下一个chunk
		var chunk *Chunk
		chunk = nil
		startOffset := p.NextChunkStartOffset
		p.NextChunkStartOffset += p.ChunkSize
		if startOffset <= p.EndOffset {
			endOffset := startOffset + p.ChunkSize - 1
			if endOffset > p.EndOffset {
				endOffset = p.EndOffset
			}
			chunk = newChunk(startOffset, endOffset)

			p.ReadyChunkQueue <- chunk
		}
		p.ProxyMutex.Unlock()

		// 所有chunk已下载完
		if chunk == nil {
			break
		}

		for {
			if !p.ProxyRunning {
				break
			} else {
				// 过多的数据未被取走，先休息一下，避免内存溢出
				remainingSize := p.GetRemainingSize(p.ChunkSize)
				maxBufferSize := p.ChunkSize * p.MaxBufferedChunk
				if remainingSize >= maxBufferSize {
					logrus.Debugf("未读取数据: %d >= 缓冲区: %d ，先休息一下，避免内存溢出", remainingSize, maxBufferSize)
					time.Sleep(1 * time.Second)
				} else {
					// logrus.Debugf("未读取数据: %d < 缓冲区: %d , 下载继续", remainingSize, maxBufferSize)
					break
				}
			}
		}

		for {
			if !p.ProxyRunning {
				break
			} else {
				// 建立连接
				rangeStr := fmt.Sprintf("bytes=%d-%d", chunk.startOffset, chunk.endOffset)
				newHeader := make(map[string][]string)
				for key, value := range req.Header {
					if !shouldFilterHeaderName(key) {
						newHeader[key] = value
					}
				}

				maxRetries := 5
				if startOffset < int64(1048576) || (p.EndOffset-startOffset)/p.EndOffset*1000 < 2 {
					maxRetries = 7
				}

				var resp *resty.Response
				var err error
				for retry := 0; retry < maxRetries; retry++ {
					resp, err = base.RestyClient.
						SetTimeout(10*time.Second).
						SetRetryCount(1).
						SetCookieJar(p.CookieJar).
						R().
						SetHeaderMultiValues(newHeader).
						SetHeader("Range", rangeStr).
						Get(p.DownloadUrl)

					if err != nil {
						logrus.Errorf("处理 %+v 链接 range=%d-%d 部分失败: %+v", p.DownloadUrl, chunk.startOffset, chunk.endOffset, err)
						time.Sleep(1 * time.Second)
						resp = nil
						continue
					}
					if !strings.HasPrefix(resp.Status(), "20") {
						logrus.Debugf("处理 %+v 链接 range=%d-%d 部分失败, statusCode: %+v: %s", p.DownloadUrl, chunk.startOffset, chunk.endOffset, resp.StatusCode(), resp.String())
						resp = nil
						p.ProxyStop()
						return
					}
					break
				}

				if err != nil {
					resp = nil
					p.ProxyStop()
					return
				}

				// 接收数据
				if resp != nil && resp.Body() != nil {
					buffer := make([]byte, chunk.endOffset-chunk.startOffset+1)
					copy(buffer, resp.Body())
					chunk.put(buffer)
				}
				resp = nil
				break
			}
		}
	}
}

func (p *ProxyDownloadStruct) GetRemainingSize(bufferSize int64) int64 {
	p.ProxyMutex.Lock()
	defer p.ProxyMutex.Unlock()
	return int64(len(p.ReadyChunkQueue)) * bufferSize
}

func handleMethod(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		// 处理 GET 请求
		logrus.Info("正在 GET 请求")
		// 检查查询参数是否为空
		if req.URL.RawQuery == "" {
			// 获取嵌入的 index.html 文件
			index, err := embedRes.Open("static/index.html")
			if err != nil {
				http.Error(w, fmt.Sprintf("读取index.html错误: %v", err), http.StatusInternalServerError)
				return
			}
			defer index.Close()

			// 将嵌入的文件内容复制到响应中
			io.Copy(w, index)

		} else {
			// 如果有查询参数，则返回自定义的内容
			handleGetMethod(w, req)
		}
	default:
		// 处理其他方法的请求
		logrus.Infof("正在处理 %v 请求", req.Method)
		handleOtherMethod(w, req)
	}
}

func handleGetMethod(w http.ResponseWriter, req *http.Request) {
	pw := bufio.NewWriterSize(w, 128*1024)
	defer func() {
		if pw.Buffered() > 0 {
			pw.Flush()
		}
	}()

	var url string
	query := req.URL.Query()
	url = query.Get("url")
	strForm := query.Get("form")
	strHeader := query.Get("header")
	strThread := req.URL.Query().Get("thread")
	strSplitSize := req.URL.Query().Get("size")
	if url != "" {
		if strForm == "base64" {
			bytesUrl, err := base64.StdEncoding.DecodeString(url)
			if err != nil {
				http.Error(w, fmt.Sprintf("无效的 Base64 Url: %v", err), http.StatusBadRequest)
				return
			}
			url = string(bytesUrl)
		}
	} else {
		http.Error(w, "缺少url参数", http.StatusBadRequest)
		return
	}

	if strHeader != "" {
		if strForm == "base64" {
			bytesStrHeader, err := base64.StdEncoding.DecodeString(strHeader)
			if err != nil {
				http.Error(w, fmt.Sprintf("无效的Base64 Headers: %v", err), http.StatusBadRequest)
				return
			}
			strHeader = string(bytesStrHeader)
		}
		var header map[string]string
		err := json.Unmarshal([]byte(strHeader), &header)
		if err != nil {
			http.Error(w, fmt.Sprintf("Header Json格式化错误: %v", err), http.StatusInternalServerError)
			return
		}
		for key, value := range header {
			req.Header.Set(key, value)
		}
	}

	newHeader := make(map[string][]string)
	for key, value := range req.Header {
		if !shouldFilterHeaderName(key) {
			newHeader[key] = value
		}
	}
	parsedURL, _ := handleUrl.Parse(url)
	newHeader["Host"] = []string{parsedURL.Host}

	for parameterName := range query {
		if parameterName == "url" || parameterName == "form" || parameterName == "thread" || parameterName == "size" || parameterName == "header" {
			continue
		}
		url = url + "&" + parameterName + "=" + query.Get(parameterName)
	}

	jar, _ := cookiejar.New(nil)
	cookies := req.Cookies()
	if len(cookies) > 0 {
		// 将 cookies 添加到 cookie jar 中
		u, _ := handleUrl.Parse(url)
		jar.SetCookies(u, cookies)
	}

	var statusCode int
	var rangeStart, rangeEnd = int64(0), int64(0)
	requestRange := req.Header.Get("Range")
	rangeRegex := regexp.MustCompile(`bytes= *([0-9]+) *- *([0-9]*)`)
	matchGroup := rangeRegex.FindStringSubmatch(requestRange)
	if matchGroup != nil {
		statusCode = 206
		rangeStart, _ = strconv.ParseInt(matchGroup[1], 10, 64)
		if len(matchGroup) > 2 {
			rangeEnd, _ = strconv.ParseInt(matchGroup[2], 10, 64)
		}
	} else {
		statusCode = 200
	}

	logrus.Debugf("请求头: %+v", newHeader)
	headersKey := url + "#Headers"
	var responseHeaders interface{}
	var connection = "keep-alive"
	responseHeaders, found := mediaCache.Get(headersKey)
	if !found {
		// 关闭 Idle 超时设置
		base.IdleConnTimeout = 0
		resp, err := base.RestyClient.
			SetTimeout(0).
			SetRetryCount(3).
			SetCookieJar(jar).
			R().
			SetDoNotParseResponse(true).
			SetOutput(os.DevNull).
			SetHeaderMultiValues(newHeader).
			SetHeader("Range", "bytes=0-1023").
			Get(url)
		if err != nil {
			http.Error(w, fmt.Sprintf("下载 %v 链接失败: %v", url, err), http.StatusInternalServerError)
			return
		}
		if resp.StatusCode() < 200 || resp.StatusCode() >= 400 {
			defer resp.RawBody().Close()
			bodyBytes, _ := io.ReadAll(resp.RawBody())
			bodyString := string(bodyBytes)
			http.Error(w, bodyString, resp.StatusCode())
			return
		}
		responseHeaders = resp.Header()

		var fileName string
		contentDisposition := strings.ToLower(responseHeaders.(http.Header).Get("Content-Disposition"))
		if contentDisposition != "" {
			regCompile := regexp.MustCompile(`^.*filename=\"([^\"]+)\".*$`)
			if regCompile.MatchString(contentDisposition) {
				fileName = regCompile.ReplaceAllString(contentDisposition, "$1")
			}
		} else {
			// 找到第一个 "?" 的索引
			queryIndex := strings.Index(url, "?")
			if queryIndex == -1 {
				// 如果没有 "?"，则提取从最后一个 "/" 到结尾的字符串
				fileName = url[strings.LastIndex(url, "/")+1:]
			} else {
				// 如果存在 "?"，则提取从最后一个 "/" 到 "?" 之间的字符串
				fileName = url[strings.LastIndex(url[:queryIndex], "/")+1 : queryIndex]
			}
		}

		contentType := responseHeaders.(http.Header).Get("Content-Type")
		if contentType == "" || contentType == "application/octet-stream" {
			if strings.HasSuffix(fileName, ".webm") {
				contentType = "video/webm"
			} else if strings.HasSuffix(fileName, ".avi") {
				contentType = "video/x-msvideo"
			} else if strings.HasSuffix(fileName, ".wmv") {
				contentType = "video/x-ms-wmv"
			} else if strings.HasSuffix(fileName, ".flv") {
				contentType = "video/x-flv"
			} else if strings.HasSuffix(fileName, ".mov") {
				contentType = "video/quicktime"
			} else if strings.HasSuffix(fileName, ".mkv") {
				contentType = "video/x-matroska"
			} else if strings.HasSuffix(fileName, ".ts") {
				contentType = "video/mp2t"
			} else if strings.HasSuffix(fileName, ".mpeg") || strings.HasSuffix(fileName, ".mpg") {
				contentType = "video/mpeg"
			} else if strings.HasSuffix(fileName, ".3gpp") || strings.HasSuffix(fileName, ".3gp") {
				contentType = "video/3gpp"
			} else if strings.HasSuffix(fileName, ".mp4") || strings.HasSuffix(fileName, ".m4s") {
				contentType = "video/mp4"
			}
			responseHeaders.(http.Header).Set("Content-Type", contentType)
		}

		contentRange := responseHeaders.(http.Header).Get("Content-Range")
		if contentRange != "" {
			matchGroup := regexp.MustCompile(`.*/([0-9]+)`).FindStringSubmatch(contentRange)
			contentSize, _ := strconv.ParseInt(matchGroup[1], 10, 64)
			responseHeaders.(http.Header).Set("Content-Length", strconv.FormatInt(contentSize, 10))
		} else {
			if resp.Size() > 0 {
				responseHeaders.(http.Header).Set("Content-Length", strconv.FormatInt(resp.Size(), 10))
			} else {
				responseHeaders.(http.Header).Set("Content-Length", strconv.FormatInt(resp.RawResponse.ContentLength, 10))
			}

		}

		acceptRange := responseHeaders.(http.Header).Get("Accept-Ranges")
		if contentRange == "" && acceptRange == "" {
			// 不支持断点续传
			logrus.Debug("不支持断点续传")
			buf := make([]byte, 1024*64)
			for {
				n, err := resp.RawBody().Read(buf)
				if n > 0 {
					// 写入数据到客户端
					_, writeErr := pw.Write(buf[:n])
					if writeErr != nil {
						http.Error(w, fmt.Sprintf("向客户端写入 Response 失败: %v", writeErr), http.StatusInternalServerError)
						return
					}
				}
				if err != nil {
					if err != io.EOF {
						logrus.Errorf("读取 Response Body 错误: %v", err)
					}
					break
				}
			}
			responseHeaders.(http.Header).Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", fileName))

			defer func() {
				if resp != nil && resp.RawBody() != nil {
					logrus.Debugf("resp.RawBody 已关闭")
					resp.RawBody().Close()
				}
			}()
		} else {
			// 支持断点续传
			logrus.Debug("支持断点续传")
			mediaCache.Set(headersKey, responseHeaders, 14400*time.Second)

			if resp != nil && resp.RawBody() != nil {
				logrus.Debugf("resp.RawBody 已关闭")
				resp.RawBody().Close()
			}
		}
	}

	acceptRange := responseHeaders.(http.Header).Get("Accept-Ranges")
	contentRange := responseHeaders.(http.Header).Get("Content-Range")
	if contentRange == "" && acceptRange == "" {
		// 不支持断点续传
		logrus.Debug("不支持断点续传-从缓存获取Headers")
		for key, values := range responseHeaders.(http.Header) {
			if strings.EqualFold(strings.ToLower(key), "connection") || strings.EqualFold(strings.ToLower(key), "proxy-connection") {
				continue
			}
			w.Header().Set(key, strings.Join(values, ","))
		}
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(statusCode)
	} else {
		// 支持断点续传
		logrus.Debug("支持断点续传-从缓存获取Headers")
		responseHeaders.(http.Header).Del("Content-Range")
		responseHeaders.(http.Header).Set("Accept-Ranges", "bytes")

		var splitSize int64
		var numTasks int64

		contentSize := int64(0)
		matchGroup = regexp.MustCompile(`.*/([0-9]+)`).FindStringSubmatch(contentRange)
		if matchGroup != nil {
			contentSize, _ = strconv.ParseInt(matchGroup[1], 10, 64)
		} else {
			contentSize, _ = strconv.ParseInt(responseHeaders.(http.Header).Get("Content-Length"), 10, 64)
		}

		if rangeEnd == int64(0) {
			rangeEnd = contentSize - 1
		}
		if rangeStart < contentSize {
			if strThread == "" {
				if contentSize < 1*1024*1024*1024 {
					numTasks = 4
				} else if contentSize < 4*1024*1024*1024 {
					numTasks = 8
				} else if contentSize < 16*1024*1024*1024 {
					numTasks = 12
				} else {
					numTasks = 16
				}
			} else {
				numTasks, _ = strconv.ParseInt(strThread, 10, 64)
			}

			if numTasks <= 0 {
				numTasks = 1
			}

			if strSplitSize != "" {
				splitSize, _ = strconv.ParseInt(strSplitSize, 10, 64)
			} else {
				splitSize = int64(128 * 1024)
			}
			responseHeaders.(http.Header).Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rangeStart, rangeEnd, contentSize))

			for key, values := range responseHeaders.(http.Header) {
				if strings.EqualFold(strings.ToLower(key), "connection") || strings.EqualFold(strings.ToLower(key), "proxy-connection") || strings.EqualFold(strings.ToLower(key), "transfer-encoding") {
					continue
				}
				if statusCode == 200 && strings.EqualFold(strings.ToLower(key), "content-range") {
					continue
				} else if statusCode == 206 && strings.EqualFold(strings.ToLower(key), "accept-ranges") {
					continue
				}
				w.Header().Set(key, strings.Join(values, ","))
			}
			if (rangeStart + splitSize*numTasks) >= (contentSize - 1) {
				w.Header().Set("Connection", "close")
			} else {
				w.Header().Set("Connection", "keep-alive")
			}
			w.WriteHeader(statusCode)

			rp, wp := io.Pipe()
			emitter := base.NewEmitter(rp, wp)

			maxChunks := int64(128*1024*1024) / splitSize
			p := newProxyDownloadStruct(url, proxyTimeout, maxChunks, splitSize, rangeStart, rangeEnd, numTasks, jar, runtime.NumGoroutine()+1)

			go ConcurrentDownload(p, url, rangeStart, rangeEnd, splitSize, numTasks, emitter, req, jar)
			io.Copy(pw, emitter)

			defer func() {
				logrus.Debugf("handleGetMethod emitter 已关闭-支持断点续传")
				if (rangeStart + splitSize*numTasks) >= (contentSize - 1) {
					mediaCache.Delete(headersKey)
				}
				p.ProxyStop()
				p = nil
				emitter.Close()
			}()
		} else {
			statusCode = 200
			connection = "close"
			mediaCache.Delete(headersKey)
			for key, values := range responseHeaders.(http.Header) {
				if strings.EqualFold(strings.ToLower(key), "connection") || strings.EqualFold(strings.ToLower(key), "proxy-connection") || strings.EqualFold(strings.ToLower(key), "transfer-encoding") {
					continue
				}
				w.Header().Del(key)
				w.Header().Set(key, strings.Join(values, ","))
			}
			w.Header().Set("Connection", connection)
			w.WriteHeader(statusCode)
		}
	}
}

func handleOtherMethod(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()

	var url string
	query := req.URL.Query()
	url = query.Get("url")
	strForm := query.Get("form")
	strHeader := query.Get("header")

	if url != "" {
		if strForm == "base64" {
			bytesUrl, err := base64.StdEncoding.DecodeString(url)
			if err != nil {
				http.Error(w, fmt.Sprintf("无效的 Base64 Url: %v", err), http.StatusBadRequest)
				return
			}
			url = string(bytesUrl)
		}
	} else {
		http.Error(w, "缺少 url 参数", http.StatusBadRequest)
		return
	}

	// 处理自定义 header
	var header map[string]string
	if strHeader != "" {
		if strForm == "base64" {
			bytesStrHeader, err := base64.StdEncoding.DecodeString(strHeader)
			if err != nil {
				http.Error(w, fmt.Sprintf("无效的Base64 Headers: %v", err), http.StatusBadRequest)
				return
			}
			strHeader = string(bytesStrHeader)
		}
		err := json.Unmarshal([]byte(strHeader), &header)
		if err != nil {
			http.Error(w, fmt.Sprintf("Header Json格式化错误: %v", err), http.StatusInternalServerError)
			return
		}
		for key, value := range header {
			req.Header.Set(key, value)
		}
	}
	newHeader := make(map[string][]string)
	for key, value := range req.Header {
		if !shouldFilterHeaderName(key) {
			newHeader[key] = value
		}
	}

	// 构建新的 URL
	for parameterName := range query {
		if parameterName == "url" || parameterName == "form" || parameterName == "thread" || parameterName == "size" || parameterName == "header" {
			continue
		}
		url = url + "&" + parameterName + "=" + query.Get(parameterName)
	}

	jar, _ := cookiejar.New(nil)
	cookies := req.Cookies()
	if len(cookies) > 0 {
		// 将 cookies 添加到 cookie jar 中
		u, _ := handleUrl.Parse(req.URL.String())
		jar.SetCookies(u, cookies)
	}

	var reqBody []byte
	// 读取请求体以记录
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
	}

	var resp *resty.Response
	var err error
	switch req.Method {
	case http.MethodPost:
		resp, err = base.RestyClient.
			SetTimeout(10 * time.Second).
			SetRetryCount(3).
			SetCookieJar(jar).
			R().
			SetBody(reqBody).
			SetHeaderMultiValues(newHeader).
			Post(url)
	case http.MethodPut:
		resp, err = base.RestyClient.
			SetTimeout(10 * time.Second).
			SetRetryCount(3).
			SetCookieJar(jar).
			R().
			SetBody(reqBody).
			SetHeaderMultiValues(newHeader).
			Put(url)
	case http.MethodOptions:
		resp, err = base.RestyClient.
			SetTimeout(10 * time.Second).
			SetRetryCount(3).
			SetCookieJar(jar).
			R().
			SetHeaderMultiValues(newHeader).
			Options(url)
	case http.MethodDelete:
		resp, err = base.RestyClient.
			SetTimeout(10 * time.Second).
			SetRetryCount(3).
			SetCookieJar(jar).
			R().
			SetHeaderMultiValues(newHeader).
			Delete(url)
	case http.MethodPatch:
		resp, err = base.RestyClient.
			SetTimeout(10 * time.Second).
			SetRetryCount(3).
			SetCookieJar(jar).
			R().
			SetHeaderMultiValues(newHeader).
			Patch(url)
	case http.MethodHead:
		resp, err = base.RestyClient.
			SetTimeout(10 * time.Second).
			SetRetryCount(3).
			SetCookieJar(jar).
			R().
			SetHeaderMultiValues(newHeader).
			Head(url)
	default:
		http.Error(w, fmt.Sprintf("无效的Method: %v", req.Method), http.StatusBadRequest)
	}

	if err != nil {
		http.Error(w, fmt.Sprintf("%v 链接 %v 失败: %v", req.Method, url, err), http.StatusInternalServerError)
		resp = nil
		return
	}
	if resp.StatusCode() < 200 || resp.StatusCode() >= 400 {
		http.Error(w, resp.Status(), resp.StatusCode())
		resp = nil
		return
	}

	// 处理响应
	w.Header().Set("Connection", "close")
	for key, values := range resp.Header() {
		w.Header().Set(key, strings.Join(values, ","))
	}
	w.WriteHeader(resp.StatusCode())
	bodyReader := bytes.NewReader(resp.Body())
	io.Copy(w, bodyReader)
}

func shouldFilterHeaderName(key string) bool {
	if len(strings.TrimSpace(key)) == 0 {
		return false
	}
	key = strings.ToLower(key)
	return key == "range" || key == "host" || key == "http-client-ip" || key == "remote-addr" || key == "accept-encoding"
}

func checkFileExists(path string) error {
    _, err := os.Stat(path)
    if os.IsNotExist(err) {
        return fmt.Errorf("文件不存在: %s", path)
    }
    return err
}

type DNSServer struct {
	Address string
	Latency time.Duration
}

// 测试 DNS 服务器响应速度
func measureDNS(server string, domain string) (time.Duration, error) {
	client := new(dns.Client)
	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(domain), dns.TypeA)

	start := time.Now()
	_, _, err := client.Exchange(msg, net.JoinHostPort(server, "53"))
	if err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// 选择最快 DNS 服务器
func FindFastestDNS(servers []string, domain string) string {
	results := make(chan DNSServer, len(servers))
	var wg sync.WaitGroup

	for _, srv := range servers {
		wg.Add(1)
		go func(server string) {
			defer wg.Done()
			latency, err := measureDNS(server, domain)
			if err == nil {
				results <- DNSServer{server, latency}
			}
		}(srv)
	}

	wg.Wait()
	close(results)

	var serverList []DNSServer
	for res := range results {
		serverList = append(serverList, res)
	}

	sort.Slice(serverList, func(i, j int) bool {
		return serverList[i].Latency < serverList[j].Latency
	})

	if len(serverList) > 0 {
		return serverList[0].Address
	}
	return ""
}

func loadConfig(cfg *Config) error {
    // 优先级：命令行参数 > 环境变量
    path := flag.String("config", os.Getenv("CONFIG_PATH"), "外部配置文件路径")
    flag.Parse()
    // 存在外部配置时优先加载
    if *path != "" {
        data, err := os.ReadFile(*path)
        if err != nil {
            return err
        }
        return json.Unmarshal(data, cfg)
    }
    // 使用嵌入的默认配置（通过FS读取）
    data, err := embedRes.ReadFile("config.json")
    if err != nil {
        return err
    }
    return json.Unmarshal(data, cfg)
}

func main() {
	var config Config
	err := loadConfig(&config)
	if err != nil {
		logrus.Fatalf("配置文件加载失败: %v", err)
	}
	
	// 设置日志级别
	if config.Debug != nil && *config.Debug {
		logrus.SetLevel(logrus.DebugLevel)
		logrus.Info("已开启 Debug 模式")
	} else {
		logrus.SetLevel(logrus.InfoLevel)
	}
	// 设置工作池选项
	if config.WorkPool != nil {
		workPool = *config.WorkPool
	} else {
		workPool = false // 默认值
	}
	// 设置端口
	port := "7779"
	if config.Port != nil {
		var portValue string
		if err := json.Unmarshal(config.Port, &portValue); err == nil {
			port = portValue // 字符串格式的端口
		} else {
			var portInt int
			if err := json.Unmarshal(config.Port, &portInt); err == nil {
				port = fmt.Sprintf("%d", portInt) // 整数格式的端口
			} else {
				logrus.Errorf("警告: 无法解析端口值，将使用默认端口: %s", port)
			}
		}
	}
	// 设置DNS
	dnsResolver := *config.DNS
	if dnsResolver == "" {
		dnsResolver = FindFastestDNS([]string{"119.29.29.29", "180.76.76.76", "223.5.5.5", "114.114.114.114", "1.1.1.1", "101.226.4.6", "1.2.4.8", "210.2.4.8", "123.125.81.6"}, "baidu.com")
		logrus.Infof("自动选择最快DNS: %s", dnsResolver)
	}

	// 忽略 SIGPIPE 信号
	signal.Ignore(syscall.SIGPIPE)

	// 设置日志输出和级别
	logrus.SetOutput(os.Stdout)

	// 设置 DNS 解析器 IP
	base.DnsResolverIP = dnsResolver + ":53"
	base.InitClient()

	// 设置http(s)服务器
	var HTTPService = true
	if config.SSL == nil || config.SSL.Key == nil || config.SSL.Cert == nil {
		logrus.Error("SSL证书不完整，启动HTTP服务")
	} else {
		keyErr := checkFileExists(*config.SSL.Key)
		certErr := checkFileExists(*config.SSL.Cert)
		if certErr != nil || keyErr != nil {
			logrus.Error("SSL证书不完整，启动HTTP服务")
		} else {
			HTTPService = false
		}
	}
	if HTTPService {
		server := http.Server{
			Addr:    ":" + port,
			Handler: http.HandlerFunc(handleMethod),
		}
		logrus.Infof("HTTP服务运行在 %s 端口.", port)
		server.ListenAndServe()
	} else {
		server := &http.Server{
			Addr:    ":" + port,
			Handler: http.HandlerFunc(handleMethod),
			TLSConfig: &tls.Config{
				MinVersion: tls.VersionTLS12, // 可选：设置支持的最低 TLS 版本
			},
		}
		logrus.Infof("HTTPS服务运行在 %s 端口.", port)
		server.ListenAndServeTLS(*config.SSL.Cert, *config.SSL.Key)
	}
}
