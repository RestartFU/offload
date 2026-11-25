package client

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RestartFU/offload/internal/api"
)

// MCP starts a session from a runner profile and bridges this process's stdio to it, so an MCP
// client can use `offload mcp <profile>` as a stdio server. Stdin lines are sent in order with
// sequence numbers (safe to retry); stdout resumes from the last byte after reconnects.
func MCP(profile, on string, res api.Resources) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	hosts := Discover(ctx, cfg)
	var h *Host
	if on != "" {
		h, err = FindHost(hosts, on)
	} else {
		h, err = Pick(hosts, res, profile, "", "session")
	}
	if err != nil {
		return err
	}
	var se api.Session
	if err := h.do(ctx, "POST", "/v1/sessions", api.SessionSpec{Profile: profile, Resources: res, Client: clientLabel()}, &se); err != nil {
		return fmt.Errorf("%s: %w", h.Name, err)
	}
	fmt.Fprintf(os.Stderr, "offload: session %s:%s (%s)\n", h.Name, se.ID, profile)
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = h.do(c, "DELETE", "/v1/sessions/"+se.ID, nil, nil)
	}()

	done := make(chan error, 3)
	go func() { done <- pumpOut(ctx, h, se.ID, "stdout", os.Stdout) }()
	go func() { _ = pumpOut(ctx, h, se.ID, "stderr", os.Stderr) }()
	go func() { done <- pumpIn(ctx, h, se.ID, os.Stdin) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-done:
		return err
	}
}

func pumpIn(ctx context.Context, h *Host, id string, in io.Reader) error {
	r := bufio.NewReaderSize(in, 1<<20)
	var seq int64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			seq++
			if serr := sendStdin(ctx, h, id, seq, line); serr != nil {
				return serr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil // the MCP client closed stdin: end the session
			}
			return err
		}
	}
}

func sendStdin(ctx context.Context, h *Host, id string, seq int64, line []byte) error {
	for attempt := 0; ; attempt++ {
		err := h.do(ctx, "POST", fmt.Sprintf("/v1/sessions/%s/stdin?seq=%d", id, seq), bytes.NewReader(line), nil)
		if err == nil {
			return nil
		}
		var ae *APIError
		if errors.As(err, &ae) && (ae.Code == 404 || ae.Code == 410) {
			return fmt.Errorf("session ended: %s", ae.Msg)
		}
		if ctx.Err() != nil || attempt > 120 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// pumpOut copies a session stream, reconnecting at the last offset, until the session ends.
func pumpOut(ctx context.Context, h *Host, id, stream string, w io.Writer) error {
	var offset int64
	for {
		resp, err := h.request(ctx, "GET", fmt.Sprintf("/v1/sessions/%s/%s?follow=1&offset=%d", id, stream, offset), nil)
		if err == nil {
			n, _ := io.Copy(w, resp.Body)
			resp.Body.Close()
			offset += n
			var se api.Session
			if gerr := h.do(ctx, "GET", "/v1/sessions/"+id, nil, &se); gerr == nil && api.Terminal(se.State) {
				if se.Reason != "" && stream == "stdout" {
					fmt.Fprintf(os.Stderr, "offload: session ended: %s\n", se.Reason)
				}
				return nil
			}
		}
		var ae *APIError
		if errors.As(err, &ae) && ae.Code == 404 {
			return fmt.Errorf("session %s is gone", id)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}
