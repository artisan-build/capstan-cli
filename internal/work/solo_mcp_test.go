//go:build !windows

package work

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStdioSoloClientFramesCallsAndBoundsFailures(t *testing.T) {
	t.Run("line framed handshake and tool call", func(t *testing.T) {
		path := writeMCPPeer(t, `
import json, sys
for line in sys.stdin:
    msg=json.loads(line)
    if 'id' not in msg: continue
    if msg['method']=='initialize': result={}
    else:
        name=msg['params']['name']
        result={'content':[{'type':'text','text':json.dumps({'name':name,'arguments':msg['params']['arguments']})}]}
    print(json.dumps({'jsonrpc':'2.0','id':msg['id'],'result':result}), flush=True)
`)
		client, err := NewStdioSoloClient(context.Background(), path, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = client.Close() }()
		var result struct {
			Name      string         `json:"name"`
			Arguments map[string]int `json:"arguments"`
		}
		if err := client.Call(context.Background(), "echo", map[string]int{"value": 7}, &result); err != nil {
			t.Fatal(err)
		}
		if result.Name != "echo" || result.Arguments["value"] != 7 {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("method errors are redacted", func(t *testing.T) {
		path := writeMCPPeer(t, `
import json, sys
for line in sys.stdin:
    msg=json.loads(line)
    if 'id' not in msg: continue
    print(json.dumps({'jsonrpc':'2.0','id':msg['id'],'error':{'code':-32000,'message':'signed-secret'}}), flush=True)
`)
		_, err := NewStdioSoloClient(context.Background(), path, time.Second)
		if err == nil || strings.Contains(err.Error(), "signed-secret") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("timeout kills hung peer", func(t *testing.T) {
		path := writeMCPPeer(t, `
import sys, time
for line in sys.stdin: time.sleep(10)
`)
		started := time.Now()
		_, err := NewStdioSoloClient(context.Background(), path, 30*time.Millisecond)
		if err == nil || time.Since(started) > time.Second {
			t.Fatalf("error, elapsed = %v, %s", err, time.Since(started))
		}
	})

	t.Run("oversized message is refused", func(t *testing.T) {
		path := writeMCPPeer(t, `
import sys
for line in sys.stdin:
    print('x' * 1100000, flush=True)
`)
		_, err := NewStdioSoloClient(context.Background(), path, time.Second)
		if err == nil {
			t.Fatal("oversized MCP response was accepted")
		}
	})
}

func writeMCPPeer(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp-peer")
	if err := os.WriteFile(path, []byte("#!/usr/bin/env python3\n"+source), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
