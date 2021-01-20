// Copyright (c) 2020 Gitpod GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package archive

import (
	"archive/tar"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/idtools"
	"github.com/gitpod-io/gitpod/common-go/tracing"
	"github.com/opentracing/opentracing-go"
	"golang.org/x/xerrors"
)

type tarConfig struct {
	MaxSizeBytes int64
	UIDMaps      []idtools.IDMap
	GIDMaps      []idtools.IDMap
}

// BuildTarbalOption configures the tarbal creation
type TarOption func(o *tarConfig)

// TarbalMaxSize limits the size of a tarbal
func TarbalMaxSize(n int64) TarOption {
	return func(o *tarConfig) {
		o.MaxSizeBytes = n
	}
}

// IDMapping maps user or group IDs
type IDMapping struct {
	ContainerID int
	HostID      int
	Size        int
}

// WithUIDMapping reverses the given user ID mapping during archive creation
func WithUIDMapping(mappings []IDMapping) TarOption {
	return func(o *tarConfig) {
		o.UIDMaps = make([]idtools.IDMap, len(mappings))
		for i, m := range mappings {
			o.UIDMaps[i] = idtools.IDMap{
				ContainerID: m.ContainerID,
				HostID:      m.HostID,
				Size:        m.Size,
			}
		}
	}
}

// WithGIDMapping reverses the given user ID mapping during archive creation
func WithGIDMapping(mappings []IDMapping) TarOption {
	return func(o *tarConfig) {
		o.GIDMaps = make([]idtools.IDMap, len(mappings))
		for i, m := range mappings {
			o.GIDMaps[i] = idtools.IDMap{
				ContainerID: m.ContainerID,
				HostID:      m.HostID,
				Size:        m.Size,
			}
		}
	}
}

// ExtractTarbal extracts an OCI compatible tar file src to the folder dst, expecting the overlay whiteout format
func ExtractTarbal(ctx context.Context, src io.Reader, dst string, opts ...TarOption) (err error) {
	var cfg tarConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	//nolint:staticcheck,ineffassign
	span, ctx := opentracing.StartSpanFromContext(ctx, "extractTarbal")
	span.LogKV("src", src, "dst", dst)
	defer tracing.FinishSpan(span, &err)

	re, wr := io.Pipe()
	src = io.TeeReader(src, wr)
	tarReader := tar.NewReader(re)
	type Info struct {
		UID, GID int
	}
	m := make(map[string]Info)
	go func() {
		for {
			hdr, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			m[hdr.Name] = Info{
				UID: hdr.Uid,
				GID: hdr.Gid,
			}
		}
	}()

	tarcmd := exec.Command("tar", "x")
	tarcmd.Dir = dst
	tarcmd.Stdin = src

	msg, err := tarcmd.CombinedOutput()
	if err != nil {
		return xerrors.Errorf("tar %s: %s", dst, err.Error()+";"+string(msg))
	}
	for k, v := range m {
		os.Chown(k, toContainerId(v.UID, cfg.UIDMaps), toContainerId(v.GID, cfg.GIDMaps))
	}
	return nil
}

func toContainerId(hostID int, idMap []idtools.IDMap) int {
	for _, m := range idMap {
		if (hostID >= m.HostID) && (hostID <= (m.HostID + m.Size - 1)) {
			contID := m.ContainerID + (hostID - m.HostID)
			return contID
		}
	}
	return hostID
}

// BuildTarbal creates an OCI compatible tar file dst from the folder src, expecting the overlay whiteout format
func BuildTarbal(ctx context.Context, src string, dst string, opts ...TarOption) (err error) {
	var cfg tarConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	//nolint:staticcheck,ineffassign
	span, ctx := opentracing.StartSpanFromContext(ctx, "buildTarbal")
	span.LogKV("src", src, "dst", dst)
	defer tracing.FinishSpan(span, &err)

	// ensure the src actually exists before trying to tar it
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("Unable to tar files: %v", err.Error())
	}

	tarout, err := archive.TarWithOptions(src, &archive.TarOptions{
		Compression:    archive.Uncompressed,
		WhiteoutFormat: archive.OverlayWhiteoutFormat,
		InUserNS:       true,
		UIDMaps:        cfg.UIDMaps,
		GIDMaps:        cfg.GIDMaps,
	})
	if err != nil {
		return xerrors.Errorf("cannot create tar: %w", err)
	}

	fout, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE, 0744)
	if err != nil {
		return xerrors.Errorf("cannot open archive for writing: %w", err)
	}
	defer fout.Close()
	fbout := bufio.NewWriter(fout)
	defer fbout.Flush()

	targetOut := newLimitWriter(fbout, cfg.MaxSizeBytes)
	defer func(e *error) {
		if targetOut.DidMaxOut() {
			*e = ErrMaxSizeExceeded
		}
	}(&err)

	_, err = io.Copy(targetOut, tarout)
	if err != nil {
		return xerrors.Errorf("cannot write tar file: %w")
	}
	if err = fbout.Flush(); err != nil {
		return xerrors.Errorf("cannot flush tar out stream: %w", err)
	}

	return nil
}

// ErrMaxSizeExceeded is emitted by LimitWriter when a write tries to write beyond the max number of bytes allowed
var ErrMaxSizeExceeded = fmt.Errorf("maximum size exceeded")

// newLimitWriter wraps a writer such that a maximum of N bytes can be written. Once that limit is exceeded
// the writer returns io.ErrClosedPipe
func newLimitWriter(out io.Writer, maxSizeBytes int64) *limitWriter {
	return &limitWriter{
		MaxSizeBytes: maxSizeBytes,
		Out:          out,
	}
}

type limitWriter struct {
	MaxSizeBytes int64
	Out          io.Writer
	BytesWritten int64

	didMaxOut bool
}

func (s *limitWriter) Write(b []byte) (n int, err error) {
	if s.MaxSizeBytes == 0 {
		return s.Out.Write(b)
	}

	bsize := int64(len(b))
	if bsize+s.BytesWritten > s.MaxSizeBytes {
		s.didMaxOut = true
		return 0, ErrMaxSizeExceeded
	}

	n, err = s.Out.Write(b)
	s.BytesWritten += int64(n)

	return n, err
}

func (s *limitWriter) DidMaxOut() bool {
	return s.didMaxOut
}
