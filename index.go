package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/phuslu/fetchserver/Godeps/_workspace/src/github.com/klauspost/compress/flate"
	"github.com/phuslu/fetchserver/Godeps/_workspace/src/github.com/klauspost/compress/gzip"
	"github.com/phuslu/fetchserver/Godeps/_workspace/src/github.com/valyala/fasthttp"
	"github.com/phuslu/fetchserver/Godeps/_workspace/src/github.com/valyala/fasthttp/reuseport"
)

const (
	Version  = "1.0"
	Password = "123456"
)

var (
	readerPool = sync.Pool{
		New: func() interface{} {
			return flate.NewReader(strings.NewReader(""))
		},
	}
	bPool = sync.Pool{
		New: func() interface{} {
			return bytes.NewBuffer(make([]byte, 0, 4<<10))
		},
	}
)

func main() {
	parts := []string{"", "8080"}

	for i, keys := range [][]string{{"VCAP_APP_HOST", "HOST"}, {"VCAP_APP_PORT", "PORT"}} {
		for _, key := range keys {
			if s := os.Getenv(key); s != "" {
				parts[i] = s
			}
		}
	}

	addr := strings.Join(parts, ":")
	fmt.Fprintf(os.Stdout, "Start ListenAndServe on %v\n", addr)

	// windows
	// if err := fasthttp.ListenAndServe(addr, handler); err != nil {
	// 		panic(err)
	// }

	listener, err := reuseport.Listen("tcp4", addr)
	if err != nil {
		panic(err)
	}
	defer listener.Close()

	if err := fasthttp.Serve(listener, handler); err != nil {
		panic(err)
	}
}

func ReadRequest(li *io.LimitedReader) (req *fasthttp.Request, err error) {
	req = fasthttp.AcquireRequest()

	r := readerPool.Get().(io.ReadCloser)
	r.(flate.Resetter).Reset(li, nil)
	defer func() {
		r.Close()
		readerPool.Put(r)
	}()

	scanner := bufio.NewScanner(r)
	if scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, " ")
		if len(parts) != 3 {
			err = fmt.Errorf("Invaild Request Line: %#v", line)
			return
		}
		req.Header.SetMethod(parts[0])
		req.SetRequestURI(parts[1])

		if u, er := url.Parse(parts[1]); er != nil {
			return
		} else {
			req.Header.Set("Host", u.Host)
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		req.Header.Set(key, value)
	}

	if err = scanner.Err(); err != nil {
		// ignore
	}

	return
}

func handler(ctx *fasthttp.RequestCtx) {
	var err error

	logger := log.New(os.Stdout, "index.go: ", 0)

	body := bPool.Get().(*bytes.Buffer) // io.ReadWriter
	body.Write(ctx.Request.Body())
	defer func() {
		body.Reset()
		bPool.Put(body)
	}()

	var hdrLen uint16
	if err := binary.Read(body, binary.BigEndian, &hdrLen); err != nil {
		parts := strings.Split(string(ctx.Host()), ".")
		switch len(parts) {
		case 1, 2:
			ctx.Error("fetchserver:"+err.Error(), fasthttp.StatusBadRequest)
		default:
			u := ctx.URI()
			if len(u.Scheme()) == 0 {
				u.SetScheme("http")
			}
			u.SetHost(fmt.Sprintf("phuslu-%d.%s", time.Now().Nanosecond(), strings.Join(parts[1:], ".")))
			if statusCode, body, err := fasthttp.Get(nil, u.String()); err == nil {
				ctx.SetStatusCode(statusCode)
				ctx.Write(body)
			} else {
				u.SetHost("www." + strings.Join(parts[1:], "."))
				ctx.Redirect(u.String(), 301)
			}
		}
		return
	}

	req1, err := ReadRequest(&io.LimitedReader{R: body, N: int64(hdrLen)})
	req1.SetBody(body.Bytes())

	if ce := string(req1.Header.Peek("Content-Encoding")); ce != "" {
		var r io.Reader
		bufb := bytes.NewBuffer(req1.Body())
		switch ce {
		case "deflate":
			r = flate.NewReader(bufb)
		case "gzip":
			if r, err = gzip.NewReader(bufb); err != nil {
				ctx.Error("fetchserver:"+err.Error(), fasthttp.StatusBadRequest)
				return
			}
		default:
			ctx.Error("fetchserver:"+fmt.Sprintf("Unsupported Content-Encoding: %#v", ce), fasthttp.StatusBadRequest)
			return
		}
		data, err := ioutil.ReadAll(r)
		if err != nil {
			req1.ResetBody()
			ctx.Error("fetchserver:"+err.Error(), fasthttp.StatusBadRequest)
			return
		}
		req1.ResetBody()
		req1.SetBody(data)
		req1.Header.Set("Content-Length", strconv.FormatInt(int64(len(data)), 10))
		req1.Header.Del("Content-Encoding")
	}

	logger.Printf("%s \"%s %s -", ctx.RemoteAddr(), string(req1.Header.Method()), string(req1.URI().FullURI()))

	var paramsPreifx string = "X-Urlfetch-"
	params := map[string]string{}
	visitHeader := func(key, value []byte) {
		if strings.HasPrefix(string(key), paramsPreifx) {
			params[strings.ToLower(string(key[len(paramsPreifx):]))] = string(value)
		}
	}
	req1.Header.VisitAll(visitHeader)

	for _, key := range params {
		req1.Header.Del(paramsPreifx + key)
	}
	if Password != "" {
		if password, ok := params["password"]; !ok || password != Password {
			ctx.Error(fmt.Sprintf("fetchserver: wrong password %#v", password), fasthttp.StatusForbidden)
			return
		}
	}

	var resp = fasthttp.AcquireResponse()
	for i := 0; i < 2; i++ {
		err = fasthttp.DoTimeout(req1, resp, 30*time.Second)
		if err == nil {
			break
		}
		if resp != nil && resp.Body() != nil {
			fasthttp.ReleaseResponse(resp)
		}

		if err1, ok := err.(interface {
			Temporary() bool
		}); ok && err1.Temporary() {
			time.Sleep(1 * time.Second)
			continue
		}
		ctx.Error("fetchserver:"+err.Error(), fasthttp.StatusBadGateway)
		return
	}
	go fasthttp.ReleaseRequest(req1)
	defer fasthttp.ReleaseResponse(resp)
	ctx.SetStatusCode(fasthttp.StatusOK)
	ctx.Write(resp.Header.Header())
	ctx.Write(resp.Body())
}
