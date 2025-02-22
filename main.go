package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

func init() {
	flag.Usage = func() {
		h := []string{
			"Request URLs provided on stdin fairly frickin' fast",
			"",
			"Options:",
			"  -b, --body <data>         Request body",
			"  -d, --delay <delay>       Delay between issuing requests (ms)",
			"  -H, --header <header>     Add a header to the request (can be specified multiple times)",
			"      --ignore-html         Don't save HTML files; useful when looking non-HTML files only",
			"      --ignore-empty        Don't save empty files",
			"  -k, --keep-alive          Use HTTP Keep-Alive",
			"  -m, --method              HTTP method to use (default: GET, or POST if body is specified)",
			"  -ms <string>              Match string that is included in the body",
			"  -mc <code>                Match status code (can be specified in comma separated format)",
			"  -fc <code>                Filter out status code (can be specified in comma separated format)",
			"  -o, --output <dir>        Directory to save responses in (will be created)",
			"  -x, --proxy <proxyURL>    Use the provided HTTP proxy",
			"",
		}

		fmt.Fprintf(os.Stderr, strings.Join(h, "\n"))
	}
}

func main() {

	var requestBody string
	flag.StringVar(&requestBody, "body", "", "")
	flag.StringVar(&requestBody, "b", "", "")

	var keepAlives bool
	flag.BoolVar(&keepAlives, "keep-alive", false, "")
	flag.BoolVar(&keepAlives, "keep-alives", false, "")
	flag.BoolVar(&keepAlives, "k", false, "")

	var delayMs int
	flag.IntVar(&delayMs, "delay", 100, "")
	flag.IntVar(&delayMs, "d", 100, "")

	var method string
	flag.StringVar(&method, "method", "GET", "")
	flag.StringVar(&method, "m", "GET", "")

	var headers headerArgs
	flag.Var(&headers, "header", "")
	flag.Var(&headers, "H", "")

	var matchString string
	flag.StringVar(&matchString, "ms", "", "")

	var matchCode statusArgs
	flag.Var(&matchCode, "mc", "")

	var filterCode statusArgs
	flag.Var(&filterCode, "exclude-status", "")
	flag.Var(&filterCode, "ex", "")

	var outputDir string
	flag.StringVar(&outputDir, "output", "", "")
	flag.StringVar(&outputDir, "o", "", "")

	var proxy string
	flag.StringVar(&proxy, "proxy", "", "")
	flag.StringVar(&proxy, "x", "", "")

	var ignoreHTMLFiles bool
	flag.BoolVar(&ignoreHTMLFiles, "ignore-html", false, "")

	var ignoreEmpty bool
	flag.BoolVar(&ignoreEmpty, "ignore-empty", false, "")

	flag.Parse()

	delay := time.Duration(delayMs * 1000000)
	client := newClient(keepAlives, proxy)
	prefix := outputDir
	if prefix == "" {
		prefix = "out"
	}

	stdoutFormatStr := "%s,%s,status: %d,size: %d,words: %d,lines: %d,type: %s\n"

	// regex for determining if something is probably HTML. You might
	// think that checking the content-type response header would be a better
	// idea, and you might be right - but if there's one thing I've learnt
	// about webservers it's that they are dirty, rotten, filthy liars.
	isHTML := regexp.MustCompile(`(?i)<html`)

	var wg sync.WaitGroup

	sc := bufio.NewScanner(os.Stdin)

	for sc.Scan() {

		rawURL := sc.Text()
		wg.Add(1)
		time.Sleep(delay)

		go func() {
			defer wg.Done()

			// create the request
			var b io.Reader
			if requestBody != "" {
				b = strings.NewReader(requestBody)

				// Can't send a body with a GET request
				if method == "GET" {
					method = "POST"
				}
			}

			_, err := url.ParseRequestURI(rawURL)
			if err != nil {
				return
			}

			req, err := http.NewRequest(method, rawURL, b)
			if err != nil {
				//fmt.Fprintf(os.Stderr, "failed to create request: %s\n", err)
				fmt.Printf(stdoutFormatStr, rawURL, err, 0, 0, 0, 0, "error")
				return
			}

			// add headers to the request
			for _, h := range headers {
				parts := strings.SplitN(h, ":", 2)

				if len(parts) != 2 {
					continue
				}
				req.Header.Set(parts[0], parts[1])
			}

			// send the request
			resp, err := client.Do(req)
			if err != nil {
				//fmt.Fprintf(os.Stderr, "request failed: %s\n", err)
				fmt.Printf(stdoutFormatStr, rawURL, err, 0, 0, 0, 0, "error")
				return
			}
			defer resp.Body.Close()

			// we want to read the body into a string or something like that so we can provide options to
			// not save content based on a pattern or something like that
			responseBody, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				//fmt.Fprintf(os.Stderr, "failed to read body: %s\n", err)
				fmt.Printf(stdoutFormatStr, rawURL, err, 0, 0, 0, 0, "error")
				return
			}

			// If we've been asked to ignore HTML files then we should really do that.
			// But why would you want to ignore HTML files? Sometimes you're looking at
			// a ton of hosts for config files and that sort of thing, and they lie to you
			// by sending a 200 response code instead of a 404. Those pages are *usually*
			// HTML so providing a way to ignore them cuts down on clutter a little bit,
			// even if it is a niche use-case.
			if ignoreHTMLFiles && isHTML.Match(responseBody) {
				return
			}

			// sometimes we don't about the response at all if it's empty
			if ignoreEmpty && len(bytes.TrimSpace(responseBody)) == 0 {
				return
			}

			// if a -M/--match option has been used, we always want to save if it matches
			if matchString != "" && !bytes.Contains(responseBody, []byte(matchString)) {
				return
			}

			if len(matchCode) > 0 && !matchCode.Includes(resp.StatusCode) {
				return
			}

			if len(filterCode) > 0 && !filterCode.Includes(resp.StatusCode) {
				return
			}

			resp.ContentLength = int64(len(string(responseBody)))
			wordsSize := len(strings.Split(string(responseBody), " "))
			linesSize := len(strings.Split(string(responseBody), "\n"))

			if outputDir == "" {
				fmt.Printf(stdoutFormatStr, rawURL, resp.Header.Get("Location"), resp.StatusCode, resp.ContentLength, wordsSize, linesSize, resp.Header.Get("Content-Type"))
				return
			}

			// output files are stored in prefix/domain/normalisedpath/hash.(body|headers)
			normalisedPath := normalisePath(req.URL)
			hash := sha1.Sum([]byte(method + rawURL + requestBody + headers.String()))
			p := path.Join(prefix, req.URL.Hostname(), normalisedPath, fmt.Sprintf("%x.body", hash))
			err = os.MkdirAll(path.Dir(p), 0750)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to create dir: %s\n", err)
				return
			}

			// write the response body to a file
			err = ioutil.WriteFile(p, responseBody, 0644)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to write file contents: %s\n", err)
				return
			}

			// create the headers file
			headersPath := path.Join(prefix, req.URL.Hostname(), normalisedPath, fmt.Sprintf("%x.headers", hash))
			headersFile, err := os.Create(headersPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to create file: %s\n", err)
				return
			}
			defer headersFile.Close()

			var buf strings.Builder

			// put the request URL and method at the top
			buf.WriteString(fmt.Sprintf("%s %s\n\n", method, rawURL))

			// add the request headers
			for _, h := range headers {
				buf.WriteString(fmt.Sprintf("> %s\n", h))
			}
			buf.WriteRune('\n')

			// add the request body
			if requestBody != "" {
				buf.WriteString(requestBody)
				buf.WriteString("\n\n")
			}

			// add the proto and status
			buf.WriteString(fmt.Sprintf("< %s %s\n", resp.Proto, resp.Status))

			// add the response headers
			for k, vs := range resp.Header {
				for _, v := range vs {
					buf.WriteString(fmt.Sprintf("< %s: %s\n", k, v))
				}
			}

			// add the response body
			_, err = io.Copy(headersFile, strings.NewReader(buf.String()))
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to write file contents: %s\n", err)
				return
			}

			// output the body filename for each URL
			fmt.Printf("%s: %s %d\n", p, rawURL, resp.StatusCode)
		}()
	}

	wg.Wait()

}

func newClient(keepAlives bool, proxy string) *http.Client {

	tr := &http.Transport{
		MaxIdleConns:      30,
		IdleConnTimeout:   time.Second,
		DisableKeepAlives: !keepAlives,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DialContext: (&net.Dialer{
			Timeout:   time.Second * 10,
			KeepAlive: time.Second,
		}).DialContext,
	}

	if proxy != "" {
		if p, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(p)
		}
	}

	re := func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &http.Client{
		Transport:     tr,
		CheckRedirect: re,
		Timeout:       time.Second * 10,
	}

}

type headerArgs []string

func (h *headerArgs) Set(val string) error {
	*h = append(*h, val)
	return nil
}

func (h headerArgs) String() string {
	return strings.Join(h, ", ")
}

type statusArgs []int

func (s *statusArgs) Set(val string) error {
	ary := strings.Split(val, ",")
	for i := range ary {
		if iVal, err := strconv.Atoi(ary[i]); err == nil {
			*s = append(*s, iVal)
		}
	}

	return nil
}

func (s statusArgs) String() string {
	return "string"
}

func (s statusArgs) Includes(search int) bool {
	for _, status := range s {
		if status == search {
			return true
		}
	}
	return false
}

func normalisePath(u *url.URL) string {
	re := regexp.MustCompile(`[^a-zA-Z0-9/._-]+`)
	return re.ReplaceAllString(u.Path, "-")
}
