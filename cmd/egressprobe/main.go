// Command egressprobe answers one question from INSIDE a VPC: what can a
// Lambda on these subnets actually reach?
//
// WHY THIS EXISTS. A route table that looks correct and a packet that arrives
// are different facts. On this build a production cut timed out reaching
// api.github.com and I was one step from calling it a NAT problem -- routes,
// NAT state, NACLs and the security group were all textbook-correct. A
// twenty-line probe in the same subnets and the same security group got HTTP
// 200 in 39ms, which ended the theory in one run and sent the investigation
// somewhere useful.
//
// ⚠️ It reports each stage SEPARATELY -- DNS, TCP, TLS, HTTP. "It timed out"
// is not a diagnosis; "DNS resolved in 5ms and the TCP connect hung" is.
package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

type stage struct {
	Stage string `json:"stage"`
	MS    int64  `json:"ms"`
	OK    string `json:"ok,omitempty"`
	Err   string `json:"err,omitempty"`
}

func timed(name string, f func() (string, error)) stage {
	t := time.Now()
	v, err := f()
	s := stage{Stage: name, MS: time.Since(t).Milliseconds()}
	if err != nil {
		s.Err = err.Error()
	} else {
		s.OK = v
	}
	return s
}

func probe(host string) []stage {
	out := []stage{
		timed(host+" · dns", func() (string, error) {
			ips, err := net.LookupHost(host)
			return strings.Join(ips, " "), err
		}),
		// ⚠️ tcp4 explicitly as well as tcp. If the VPC has no IPv6 route a
		// dialer that prefers AAAA can stall, and the two results together say
		// so immediately instead of looking like a slow network.
		timed(host+" · tcp", func() (string, error) {
			c, err := net.DialTimeout("tcp", host+":443", 8*time.Second)
			if err != nil {
				return "", err
			}
			defer c.Close()
			return c.RemoteAddr().String(), nil
		}),
		timed(host+" · tcp4", func() (string, error) {
			c, err := (&net.Dialer{Timeout: 8 * time.Second}).Dial("tcp4", host+":443")
			if err != nil {
				return "", err
			}
			defer c.Close()
			return c.RemoteAddr().String(), nil
		}),
		timed(host+" · tls", func() (string, error) {
			c, err := tls.DialWithDialer(&net.Dialer{Timeout: 8 * time.Second}, "tcp", host+":443", nil)
			if err != nil {
				return "", err
			}
			defer c.Close()
			return c.ConnectionState().PeerCertificates[0].Subject.CommonName, nil
		}),
		timed(host+" · https", func() (string, error) {
			r, err := (&http.Client{Timeout: 8 * time.Second}).Get("https://" + host + "/")
			if err != nil {
				return "", err
			}
			defer r.Body.Close()
			io.Copy(io.Discard, io.LimitReader(r.Body, 256))
			return r.Status, nil
		}),
	}
	return out
}

func main() {
	api := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	base := "http://" + api + "/2018-06-01/runtime"
	region := os.Getenv("AWS_REGION")

	// What ReleaseBot actually depends on. An endpoint reached PRIVATELY still
	// answers here -- the point is reachability, not which path it took.
	hosts := []string{
		"api.github.com",
		"slack.com",
		"ssm." + region + ".amazonaws.com",
		"lambda." + region + ".amazonaws.com",
		"dynamodb." + region + ".amazonaws.com",
	}
	if extra := os.Getenv("EXTRA_HOSTS"); extra != "" {
		hosts = append(hosts, strings.Split(extra, ",")...)
	}

	for {
		resp, err := http.Get(base + "/invocation/next")
		if err != nil {
			return
		}
		id := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		var all []stage
		for _, h := range hosts {
			all = append(all, probe(strings.TrimSpace(h))...)
		}
		b, _ := json.Marshal(all)
		fmt.Println("EGRESSPROBE " + string(b))
		http.Post(base+"/invocation/"+id+"/response", "application/json", strings.NewReader(string(b)))
	}
}
