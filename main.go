// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/net/context/ctxhttp"
)

const (
	// We download golang.org/toolchain version v0.0.1-<gotoolchain>.<goos>-<goarch>.
	// If the 0.0.1 indicates anything at all, its the version of the toolchain packaging:
	// if for some reason we needed to change the way toolchains are packaged into
	// module zip files in a future version of Go, we could switch to v0.0.2 and then
	// older versions expecting the old format could use v0.0.1 and newer versions
	// would use v0.0.2. Of course, then we'd also have to publish two of each
	// module zip file. It's not likely we'll ever need to change this.
	gotoolchainModule  = "golang.org/toolchain"
	gotoolchainVersion = "v0.0.1"
)

const notice = `
The go command by default downloads and authenticates modules
using the Go module mirror and Go checksum database run by Google.
See https://proxy.golang.org/privacy for privacy information
about these services and the go command documentation for configuration
details including how to disable the use of these servers or
use different ones.
`

func main() {
	ctx := context.Background()

	hostOS, hostArch, err := hostOSArch()
	if err != nil {
		log.Fatal(err)
	}

	// TODO: quiet mode

	fmt.Printf("Installing Go for %v/%v...\n", hostOS, hostArch)

	fmt.Print(notice)
	answer := "Y"
	fmt.Scanf("Do you want to continue? (Y/n)", &answer)
	if answer != "Y" {
		fmt.Println("Stopping go installation")
		os.Exit(0)
	}

	dst := installDir()
	fmt.Printf("Go will be installed in %v.\n", dst)
	fmt.Scanf("Continue? (Y/n)", &answer)
	if answer != "Y" {
		fmt.Println("Stopping go installation")
		os.Exit(0)
	}

	ver := fmt.Sprintf("v0.0.1-go1.21.0beta1-installer.%v-%v", hostOS, hostArch)
	gobin := filepath.Join(dst, "bin", "go")
	if _, err := os.Stat(gobin); err != nil {
		uri := fmt.Sprintf("https://github.com/hyangah/goup/raw/main/res/%v.zip", ver)
		r, err := ReadZip(ctx, uri)
		if err != nil {
			panic(err)
		}
		WriteZip(ctx, dst, r)
	}
	// TODO: lookup the latest version and install it.

	goCommand(gobin, "toolchain", "use", "go1.20.2")
	goCommand(gobin, "version")

	fmt.Printf("Go is installed in %v. Make sure it is in your PATH.\n", gobin)
}

func hostOSArch() (host, arch string, _ error) {
	// TODO: handle incorrect GOARCH mode (https://github.com/go-delve/delve/blob/a61ccea65a14a1640e04847e6ce11fbc8b7a0178/pkg/proc/macutil/rosetta_darwin.go#L10)
	return runtime.GOOS, runtime.GOARCH, nil
}
func installDir() string {
	if dst := os.Getenv("GOINSTALLDIR"); dst != "" {
		return dst
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".go")
}

func readBody(ctx context.Context, u string) ([]byte, error) {
	var data []byte
	err := executeRequest(ctx, u, func(body io.Reader) error {
		var err error
		data, err = io.ReadAll(body)
		return err
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// executeRequest executes an HTTP GET request for u, then calls the bodyFunc
// on the response body, if no error occurred.
func executeRequest(ctx context.Context, u string, bodyFunc func(body io.Reader) error) (err error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return err
	}
	r, err := ctxhttp.Do(ctx, nil, req)
	if err != nil {
		return fmt.Errorf("ctxhttp.Do(ctx, client, %q): %v", u, err)
	}
	defer r.Body.Close()
	if err := responseError(r, false); err != nil {
		return err
	}
	return bodyFunc(r.Body)
}

// responseError translates the response status code to an appropriate error.
func responseError(r *http.Response, fetchDisabled bool) error {
	switch {
	case 200 <= r.StatusCode && r.StatusCode < 300:
		return nil
	case 500 <= r.StatusCode:
		return fmt.Errorf("internal server error")
	case r.StatusCode == http.StatusNotFound,
		r.StatusCode == http.StatusGone:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return fmt.Errorf("io.ReadAll: %v", err)
		}
		d := string(data)
		switch {
		case strings.Contains(d, "fetch timed out"):
			err = fmt.Errorf("timeout")
		case fetchDisabled:
			err = fmt.Errorf("not fetched")
		default:
			err = fmt.Errorf("not found")
		}
		return fmt.Errorf("%q: %w", d, err)
	default:
		return fmt.Errorf("unexpected status %d %s", r.StatusCode, r.Status)
	}
}

func ReadZip(ctx context.Context, u string) (*zip.Reader, error) {
	bodyBytes, err := readBody(ctx, u)
	if err != nil {
		return nil, err
	}
	zipReader, err := zip.NewReader(bytes.NewReader(bodyBytes), int64(len(bodyBytes)))
	if err != nil {
		return nil, err
	}
	return zipReader, nil
}

func WriteZip(ctx context.Context, dst string, archive *zip.Reader) {
	_ = os.MkdirAll(dst, os.ModeDir|os.ModePerm)
	for _, f := range archive.File {
		filePath := filepath.Join(dst, f.Name)

		if !strings.HasPrefix(filePath, filepath.Clean(dst)+string(os.PathSeparator)) {
			fmt.Println("invalid file path")
			return
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(filePath, os.ModePerm)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(filePath), os.ModePerm); err != nil {
			panic(err)
		}

		dstFile, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			panic(err)
		}

		fileInArchive, err := f.Open()
		if err != nil {
			panic(err)
		}

		if _, err := io.Copy(dstFile, fileInArchive); err != nil {
			panic(err)
		}

		dstFile.Close()
		fileInArchive.Close()
	}
}

func setExecutable(gotoolchain, dir string) {
	// On first use after download, set the execute bits on the commands
	// so that we can run them. Note that multiple go commands might be
	// doing this at the same time, but if so no harm done.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, "bin/go"))
		if err != nil {
			log.Fatalf("download %s: %v", gotoolchain, err)
		}
		if info.Mode()&0111 == 0 {
			// allowExec sets the exec permission bits on all files found in dir.
			allowExec := func(dir string) {
				err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if !d.IsDir() {
						info, err := os.Stat(path)
						if err != nil {
							return err
						}
						if err := os.Chmod(path, info.Mode()&0777|0111); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					log.Fatalf("download %s: %v", gotoolchain, err)
				}
			}

			// Set the bits in pkg/tool before bin/go.
			// If we are racing with another go command and do bin/go first,
			// then the check of bin/go above might succeed, the other go command
			// would skip its own mode-setting, and then the go command might
			// try to run a tool before we get to setting the bits on pkg/tool.
			// Setting pkg/tool before bin/go avoids that ordering problem.
			// The only other tool the go command invokes is gofmt,
			// so we set that one explicitly before handling bin (which will include bin/go).
			allowExec(filepath.Join(dir, "pkg/tool"))
			allowExec(filepath.Join(dir, "bin/gofmt"))
			allowExec(filepath.Join(dir, "bin"))
		}
	}
}

func goCommand(bin string, args ...string) {
	c := exec.Command(bin, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err := c.Run()

	if err != nil {
		panic(err)
	}
}
