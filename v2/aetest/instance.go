package aetest

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"google.golang.org/appengine/v2"
	"google.golang.org/appengine/v2/internal"
)

// Instance represents a running instance of the development API Server.
type Instance interface {
	// Close kills the child api_server.py process, releasing its resources.
	io.Closer
	// NewRequest returns an *http.Request associated with this instance.
	NewRequest(method, urlStr string, body io.Reader) (*http.Request, error)
}

// Options is used to specify options when creating an Instance.
type Options struct {
	// AppID specifies the App ID to use during tests.
	// By default, "testapp".
	AppID string
	// StronglyConsistentDatastore is whether the local datastore should be
	// strongly consistent. This will diverge from production behaviour.
	StronglyConsistentDatastore bool
	// SupportDatastoreEmulator is whether use Cloud Datastore Emulator or
	// use old SQLite based Datastore backend or use default settings.
	SupportDatastoreEmulator *bool
	// SuppressDevAppServerLog is whether the dev_appserver running in tests
	// should output logs.
	SuppressDevAppServerLog bool
	// StartupTimeout is a duration to wait for instance startup.
	// By default, 15 seconds.
	StartupTimeout time.Duration
}

// NewContext starts an instance of the development API server, and returns
// a context that will route all API calls to that server, as well as a
// closure that must be called when the Context is no longer required.
func NewContext() (context.Context, func(), error) {
	inst, err := NewInstance(nil)
	if err != nil {
		return nil, nil, err
	}
	req, err := inst.NewRequest("GET", "/", nil)
	if err != nil {
		inst.Close()
		return nil, nil, err
	}
	ctx := appengine.NewContext(req)
	return ctx, func() {
		inst.Close()
	}, nil
}

// PrepareDevAppserver is a hook which, if set, will be called before the
// dev_appserver.py is started, each time it is started. If aetest.NewContext
// is invoked from the goapp test tool, this hook is unnecessary.
var PrepareDevAppserver func() error

// NewInstance launches a running instance of api_server.py which can be used
// for multiple test Contexts that delegate all App Engine API calls to that
// instance.
// If opts is nil the default values are used.
func NewInstance(opts *Options) (Instance, error) {
	i := &instance{
		opts:           opts,
		appID:          "testapp",
		startupTimeout: 15 * time.Second,
	}
	if opts != nil {
		if opts.AppID != "" {
			i.appID = opts.AppID
		}
		if opts.StartupTimeout > 0 {
			i.startupTimeout = opts.StartupTimeout
		}
	}
	if err := i.startChild(); err != nil {
		return nil, err
	}
	return i, nil
}

func newSessionID() string {
	var buf [16]byte
	io.ReadFull(rand.Reader, buf[:])
	return fmt.Sprintf("%x", buf[:])
}

// instance implements the Instance interface.
type instance struct {
	opts           *Options
	child          *exec.Cmd
	apiURL         *url.URL // base URL of API HTTP server
	adminURL       string   // base URL of admin HTTP server
	appDir         string
	appID          string
	startupTimeout time.Duration
}

// NewRequest returns an *http.Request associated with this instance.
func (i *instance) NewRequest(method, urlStr string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, urlStr, body)
	if err != nil {
		return nil, err
	}

	// Associate this request.
	return internal.RegisterTestRequest(req, i.apiURL, "dev~"+i.appID), nil
}

// Close kills the child api_server.py process, releasing its resources.
func (i *instance) Close() (err error) {
	child := i.child
	if child == nil {
		return nil
	}
	defer func() {
		i.child = nil
		err1 := os.RemoveAll(i.appDir)
		if err == nil {
			err = err1
		}
	}()

	if p := child.Process; p != nil {
		errc := make(chan error, 1)
		go func() {
			errc <- child.Wait()
		}()

		// Call the quit handler on the admin server.
		res, err := http.Get(i.adminURL + "/quit")
		if err != nil {
			p.Kill()
			return fmt.Errorf("unable to call /quit handler: %v", err)
		}
		res.Body.Close()
		select {
		case <-time.After(15 * time.Second):
			p.Kill()
			return errors.New("timeout killing child process")
		case err = <-errc:
			// Do nothing.
		}
	}
	return
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func findPython() (path string, err error) {
	for _, name := range []string{"python3"} {
		path, err = exec.LookPath(name)
		if err == nil {
			return
		}
	}
	return
}

func findDevAppserver() (string, error) {
	if p := os.Getenv("APPENGINE_DEV_APPSERVER"); p != "" {
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("invalid APPENGINE_DEV_APPSERVER environment variable; path %q doesn't exist", p)
	}
	return exec.LookPath("dev_appserver.py")
}

var apiServerAddrRE = regexp.MustCompile(`Starting API server at: (\S+)`)
var adminServerAddrRE = regexp.MustCompile(`Starting admin server at: (\S+)`)

func (i *instance) startChild() (err error) {
	if PrepareDevAppserver != nil {
		if err := PrepareDevAppserver(); err != nil {
			return err
		}
	}
	executable := os.Getenv("APPENGINE_DEV_APPSERVER_BINARY")
	var appserverArgs []string
	if len(executable) == 0 {
		executable, err = findPython()
		if err != nil {
			return fmt.Errorf("Could not find python interpreter: %v", err)
		}
		devAppserver, err := findDevAppserver()
		if err != nil {
			return fmt.Errorf("Could not find dev_appserver.py: %v", err)
		}
		appserverArgs = append(appserverArgs, devAppserver)
	}

	i.appDir, err = ioutil.TempDir("", "appengine-aetest")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(i.appDir)
		}
	}()
	err = os.Mkdir(filepath.Join(i.appDir, "app"), 0755)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filepath.Join(i.appDir, "app", "app.yaml"), []byte(i.appYAML()), 0644)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(filepath.Join(i.appDir, "app", "stubapp.go"), []byte(appSource), 0644)
	if err != nil {
		return err
	}

	datastorePath := os.Getenv("APPENGINE_DEV_APPSERVER_DATASTORE_PATH")
	if len(datastorePath) == 0 {
		datastorePath = filepath.Join(i.appDir, "datastore")
	}

	appserverArgs = append(appserverArgs,
		"--port=0",
		"--api_port=0",
		"--admin_port=0",
		"--automatic_restart=false",
		"--skip_sdk_update_check=true",
		"--clear_datastore=true",
		"--clear_search_indexes=true",
		"--datastore_path", datastorePath,
	)
	if i.opts != nil && i.opts.StronglyConsistentDatastore {
		appserverArgs = append(appserverArgs, "--datastore_consistency_policy=consistent")
	}
	if i.opts != nil && i.opts.SupportDatastoreEmulator != nil {
		appserverArgs = append(appserverArgs, fmt.Sprintf("--support_datastore_emulator=%t", *i.opts.SupportDatastoreEmulator))
	}
	appserverArgs = append(appserverArgs, filepath.Join(i.appDir, "app"))

	i.child = exec.Command(executable, appserverArgs...)

	i.child.Stdout = os.Stdout
	var stderr io.Reader
	stderr, err = i.child.StderrPipe()
	if err != nil {
		return err
	}

	if err = i.child.Start(); err != nil {
		return err
	}

	// Read stderr until we have read the URLs of the API server and admin interface.
	errc := make(chan error, 1)
	go func() {
		s := bufio.NewScanner(stderr)
		for s.Scan() {
			// Pass stderr along as we go so the user can see it.
			if !(i.opts != nil && i.opts.SuppressDevAppServerLog) {
				fmt.Fprintln(os.Stderr, s.Text())
			}
			if match := apiServerAddrRE.FindStringSubmatch(s.Text()); match != nil {
				u, err := url.Parse(match[1])
				if err != nil {
					errc <- fmt.Errorf("failed to parse API URL %q: %v", match[1], err)
					return
				}
				i.apiURL = u
			}
			if match := adminServerAddrRE.FindStringSubmatch(s.Text()); match != nil {
				i.adminURL = match[1]
			}
			if i.adminURL != "" && i.apiURL != nil {
				// Pass along stderr to the user after we're done with it.
				if !(i.opts != nil && i.opts.SuppressDevAppServerLog) {
					go io.Copy(os.Stderr, stderr)
				}
				break
			}
		}
		errc <- s.Err()
	}()

	select {
	case <-time.After(i.startupTimeout):
		if p := i.child.Process; p != nil {
			p.Kill()
		}
		return errors.New("timeout starting child process")
	case err := <-errc:
		if err != nil {
			return fmt.Errorf("error reading child process stderr: %v", err)
		}
	}
	if i.adminURL == "" {
		return errors.New("unable to find admin server URL")
	}
	if i.apiURL == nil {
		return errors.New("unable to find API server URL")
	}
	return nil
}

func (i *instance) appYAML() string {
	return fmt.Sprintf(appYAMLTemplate, i.appID)
}

const appYAMLTemplate = `
application: %s
version: 1
runtime: go122

handlers:
- url: /.*
  script: _go_app
`

const appSource = `
package main
import "google.golang.org/appengine/v2"
func main() { appengine.Main() }
`
