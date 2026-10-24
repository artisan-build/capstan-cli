package work

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const maxMCPMessageBytes = 1 << 20

type soloRPCError struct {
	code int
}

func (e soloRPCError) Error() string {
	return fmt.Sprintf("solo MCP method failed (%d)", e.code)
}

type rpcMessage struct {
	ID     int64           `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// StdioSoloClient is a bounded JSON-RPC client for Solo's stdio MCP process.
type StdioSoloClient struct {
	command   *exec.Cmd
	stdin     io.WriteCloser
	messages  chan rpcMessage
	readError chan error
	waitDone  chan struct{}
	timeout   time.Duration
	mu        sync.Mutex
	nextID    int64
	closed    bool
}

// NewStdioSoloClient starts and identifies an external Capstan MCP actor.
func NewStdioSoloClient(ctx context.Context, path string, timeout time.Duration) (*StdioSoloClient, error) {
	if path == "" || timeout <= 0 {
		return nil, errors.New("solo MCP configuration is invalid")
	}
	command := exec.CommandContext(ctx, path)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, errors.New("solo MCP stdin could not be opened")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, errors.New("solo MCP stdout could not be opened")
	}
	if err := command.Start(); err != nil {
		return nil, errors.New("solo MCP process could not be started")
	}
	client := &StdioSoloClient{
		command: command, stdin: stdin, messages: make(chan rpcMessage, 8), readError: make(chan error, 1), waitDone: make(chan struct{}), timeout: timeout,
	}
	go func() {
		_ = command.Wait()
		close(client.waitDone)
	}()
	go client.read(stdout)
	var initialized json.RawMessage
	if err := client.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "capstan-work", "version": "1"},
	}, &initialized); err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := client.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		_ = client.Close()
		return nil, err
	}
	if err := client.Call(ctx, "identify_session", map[string]any{
		"external": map[string]string{"name": "capstan-work", "agent_id": "capstan-work"},
	}, nil); err != nil {
		_ = client.Close()
		return nil, err
	}

	return client, nil
}

func (c *StdioSoloClient) read(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), maxMCPMessageBytes)
	for scanner.Scan() {
		var message rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			select {
			case c.readError <- errors.New("solo MCP returned invalid JSON-RPC"):
			default:
			}
			return
		}
		if message.ID != 0 {
			c.messages <- message
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case c.readError <- err:
	default:
	}
}

func (c *StdioSoloClient) rpc(ctx context.Context, method string, params any, target any) error {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("solo MCP client is closed")
	}
	c.nextID++
	id := c.nextID
	request := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	data, err := json.Marshal(request)
	if err != nil {
		return errors.New("solo MCP request could not be encoded")
	}
	data = append(data, '\n')
	if err := c.writeLocked(callCtx, data); err != nil {
		return err
	}
	for {
		select {
		case <-callCtx.Done():
			c.abortLocked()
			return errors.New("solo MCP request timed out")
		case <-c.readError:
			return errors.New("solo MCP closed before replying")
		case message := <-c.messages:
			if message.ID != id {
				continue
			}
			if message.Error != nil {
				return soloRPCError{code: message.Error.Code}
			}
			if target == nil {
				return nil
			}
			if err := json.Unmarshal(message.Result, target); err != nil {
				return errors.New("solo MCP result has an invalid shape")
			}
			return nil
		}
	}
}

func (c *StdioSoloClient) notify(ctx context.Context, method string, params any) error {
	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("solo MCP client is closed")
	}
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return errors.New("solo MCP notification could not be encoded")
	}
	return c.writeLocked(callCtx, append(data, '\n'))
}

func (c *StdioSoloClient) writeLocked(ctx context.Context, data []byte) error {
	written := make(chan error, 1)
	go func() {
		_, err := c.stdin.Write(data)
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			return errors.New("solo MCP request could not be written")
		}
		return nil
	case <-ctx.Done():
		c.abortLocked()
		<-written
		return errors.New("solo MCP request timed out")
	}
}

func (c *StdioSoloClient) abortLocked() {
	if !c.closed {
		c.closed = true
		_ = c.stdin.Close()
		_ = c.command.Process.Kill()
	}
	select {
	case <-c.waitDone:
	case <-time.After(2 * time.Second):
	}
}

// Call invokes one Solo MCP tool and decodes its text result as JSON.
func (c *StdioSoloClient) Call(ctx context.Context, name string, arguments any, target any) error {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result); err != nil {
		return err
	}
	if result.IsError {
		return errors.New("solo MCP tool call failed")
	}
	text := ""
	for _, content := range result.Content {
		if content.Type == "text" {
			text += content.Text
		}
	}
	if target == nil {
		return nil
	}
	if raw, ok := target.(*string); ok {
		*raw = text
		return nil
	}
	if len(text) > maxMCPMessageBytes || json.Unmarshal([]byte(text), target) != nil {
		return errors.New("solo MCP tool result has an invalid shape")
	}
	return nil
}

// Close terminates the disposable stdio MCP process.
func (c *StdioSoloClient) Close() error {
	c.mu.Lock()
	c.abortLocked()
	c.mu.Unlock()
	return nil
}
