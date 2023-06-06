package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dreamacro/clash/adapter"
	C "github.com/Dreamacro/clash/constant"
	"golang.org/x/net/context"
)

func dump(data interface{}) {
	b, _ := json.MarshalIndent(data, "", "  ")
	fmt.Print(string(b))
}

func urlToMetadata(rawURL string) (addr C.Metadata, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}

	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			err = fmt.Errorf("%s scheme not Support", rawURL)
			return
		}
	}

	addr = C.Metadata{
		Host:    u.Hostname(),
		DstIP:   netip.Addr{},
		DstPort: port,
	}
	return
}

func pingWG(ip string, port int, timeout int) (ping uint16) {
	ping = 0

	rawProxy := map[string]interface{}{
		"name":        "-",
		"type":        "wireguard",
		"server":      ip,
		"port":        port,
		"ip":          "172.16.0.2",
		"ipv6":        "2606:4700:110:84b1:b7a1:7ff8:4e7a:a9d8",
		"public-key":  "bmXOC+F1FxEMF9dyiK2H5/1SUtzH0JuVo51h2wPfgyo=",
		"private-key": privateKey,
		"mtu":         1280,
		"udp":         true,
	}

	//dump(rawProxy)
	parsedProxy, err := adapter.ParseProxy(rawProxy)
	if err != nil {
		fmt.Println("\nError 1: " + err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*time.Duration(timeout))
	defer cancel()
	start := time.Now()

	addr, err := urlToMetadata(testUrl)
	if err != nil {
		return
	}

	instance, err := parsedProxy.DialContext(ctx, &addr)
	if err != nil {
		return
	}
	defer func() {
		_ = instance.Close()
	}()

	req, err := http.NewRequest("GET", testUrl, nil)
	if err != nil {
		return
	}
	req = req.WithContext(ctx)

	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return instance, nil
		},
		// from http.DefaultTransport
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	defer client.CloseIdleConnections()

	resp, err := client.Do(req)

	if err != nil {
		return
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode == 403 {
		return
	}
	body := string(bodyBytes)

	if !strings.Contains(body, "warp=on") && !strings.Contains(body, "warp=plus") {
		return
	}

	fmt.Println("body: " + strings.Replace(body, "\n", " , ", -1))
	ping = uint16(time.Since(start) / time.Millisecond)

	return
}

func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func getCidrIps(cidr string) []string {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		fmt.Println("Error 2: " + err.Error())
		return []string{}
	}

	var ips []string
	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		ips = append(ips, ip.String())
	}
	// remove network address and broadcast address
	return ips
}

var testUrl = "https://cloudflare.com/cdn-cgi/trace"
var timeout = 2000
var concurrency = 16
var port = 2408
var pingCount = 2
var privateKey = "+JUDXOA6DJGkdhXR8oDx9BDtUOQeRvjwwLgUV3gZy1I="

var cidrs = "188.114.99.0/24 188.114.98.0/24 188.114.97.0/24 188.114.96.0/24 162.159.195.0/24 162.159.193.0/24 162.159.192.0/24"

// var cidrs = "188.114.98.0/24 188.114.99.0/24"

func main() {
	fmt.Print("Generating ip list ... ")
	ipList := []string{}
	for _, cidr := range strings.Split(cidrs, " ") {
		ipList = append(ipList, getCidrIps(cidr)...)
	}

	fmt.Printf("%d pcs has been generated.\n", len(ipList))
	// ipList = getCidrIps("188.114.98.0/24")
	// ipList, err := getCidrIps("188.114.98.224/32")

	wg := &sync.WaitGroup{}
	for _, _ip := range ipList {
		wg.Add(1)
		go func(ip string) {
			defer func() { //catch or finally
				if err := recover(); err != nil { //catch
					fmt.Fprintf(os.Stderr, "Exception: %v\n", err)

				}

			}()
			defer wg.Done()

		}(_ip)

	}
	wg.Wait()

	fmt.Println("Intializing workers ...")
	worker := func(id int, inputCh <-chan string, resultsCh chan<- int) {

		for ip := range inputCh {
			//fmt.Println("worker", id, "started  job")

			var ping int = 0

			//startTime := time.Now()
			pingCh := make(chan int, 1)

			go func() {
				defer func() { //catch or finally
					if err := recover(); err != nil { //catch
						fmt.Fprintf(os.Stderr, "Exception: %v\n", err)
						pingCh <- 0
					}
				}()

				//fmt.Print("Ping " + ip + " ... ")
				ping := 0
				for i := 1; i <= pingCount; i++ {
					//time.Sleep(time.Millisecond * 200)
					p := int(pingWG(ip, port, timeout))
					//fmt.Printf("%dms\n", p)
					if p == 0 {
						//ping = 0
						//break
					}
					if p > 0 {
						ping += p / pingCount
					}

				}

				if ping > 0 {
					//ping = ping / pingCount
				}

				pingStr := fmt.Sprintf("%dms", ping)
				if ping == 0 {
					pingStr = "(timeout)"
				}

				//fmt.Println(pingStr)
				if ping > 0 {
					fmt.Printf("%-18s -> %s\n", ip+":"+strconv.Itoa(port), pingStr)
				} else {
					//fmt.Printf("Ping %-12s -> %s\n", ip, pingStr)
					// fmt.Print(".")
				}

				pingCh <- ping
			}()

			//second timeout mechanism
			select {
			case ping = <-pingCh:

			case <-time.After(time.Millisecond * time.Duration(timeout)):
				ping = 0
				//fmt.Println("timeout ")
			}

			//elapsed := time.Since(startTime)

			resultsCh <- ping

		}
	}

	numJobs := len(ipList)
	inputCh := make(chan string, numJobs)
	resultsCh := make(chan int, numJobs)

	for w := 1; w <= concurrency; w++ {
		go worker(w, inputCh, resultsCh)
	}

	for _, ip := range ipList {
		inputCh <- ip
	}

	fmt.Println("Ping ...")
	for range ipList {
		<-resultsCh
	}
	close(inputCh)

	return

}
