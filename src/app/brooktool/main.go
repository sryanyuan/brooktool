package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

var (
	flagServer   string
	flagPassword string
	flagBinPath  string
	flagPAC      string
	flagUpdate   bool
)

const (
	defaultPAC = "https://blackwhite.txthinking.com/white.pac"
)

var (
	brookBinFile = "brook"
	pacLocalPath = "./pac.txt"
)

func init() {
	if runtime.GOOS == "windows" {
		brookBinFile += ".exe"
	}
}

func searchExecutableFile(dir string) (string, error) {
	rd, err := ioutil.ReadDir(dir)
	if nil != err {
		return "", err
	}
	for _, fi := range rd {
		if fi.IsDir() {
			path, err := searchExecutableFile(filepath.Join(dir, fi.Name()))
			if nil != err {
				return "", err
			}
			if path != "" {
				return path, nil
			}
			continue
		}
		if fi.Name() == brookBinFile {
			return filepath.Join(dir, fi.Name()), nil
		}
	}
	return "", nil
}

func Exists(path string) bool {
	_, err := os.Stat(path)
	if err != nil {
		if os.IsExist(err) {
			return true
		}
		return false
	}
	return true
}

type pacHandler struct {
	mu      sync.Mutex
	pacData []byte
}

func (h *pacHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.RequestURI, "/pac") {
		h.mu.Lock()
		data := h.pacData
		h.mu.Unlock()
		w.Write(data)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (h *pacHandler) reload() error {
	data, err := ioutil.ReadFile(pacLocalPath)
	if nil != err {
		return fmt.Errorf("Failed to reload pac: %v", err)
	}
	h.mu.Lock()
	h.pacData = data
	h.mu.Unlock()
	return nil
}

func enableSystemProxy(pacURL string) error {
	out := bytes.NewBuffer(nil)
	brookCmd := exec.Command(flagBinPath, "systemproxy",
		"--url", pacURL)
	brookCmd.Stdout = out
	brookCmd.Stderr = out
	if err := brookCmd.Start(); nil != err {
		return err
	}
	if out.Len() != 0 {
		return errors.New(out.String())
	}
	return nil
}

func main() {
	// Parsing configs
	flag.StringVar(&flagBinPath, "bin", "", "Brook executable file path")
	flag.StringVar(&flagPAC, "pac", "", "PAC file path")
	flag.StringVar(&flagServer, "server", "", "Brook server address")
	flag.StringVar(&flagPassword, "password", "", "Brook server password")
	flag.BoolVar(&flagUpdate, "update", false, "Force to update the local pac file")
	flag.Parse()

	if "" == flagServer || "" == flagPassword {
		flag.PrintDefaults()
		return
	}

	if "" == flagBinPath {
		// Search for current directory
		dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if nil != err {
			log.Fatalf("Can't get current diretory: %v", err)
			return
		}
		flagBinPath, err = searchExecutableFile(dir)
		if nil != err {
			log.Fatal("Can't find brook binary file")
			return
		}
		if "" == flagBinPath {
			log.Fatal("brook binary file not found")
			return
		}
	}
	if "" == flagPAC {
		flagPAC = defaultPAC
	}

	if !Exists(pacLocalPath) || flagUpdate {
		// Download pac file
		log.Printf("Downloading pac file from %s ...", pacLocalPath)
		rsp, err := http.Get(flagPAC)
		if nil != err {
			log.Fatalf("Can't download pac file: %v", err)
			return
		}
		defer rsp.Body.Close()

		data, err := ioutil.ReadAll(rsp.Body)
		if nil != err || 0 == len(data) {
			log.Fatalf("Download pac file error: %v", err)
			return
		}
		if err = ioutil.WriteFile(pacLocalPath, data, 0644); nil != err {
			log.Fatalf("Save pac file error: %v", err)
			return
		}
	} else {
		log.Printf("Using the cached pac file, run with -update argument to force update the pac file")
	}

	// Launch local http server to serve pac file
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if nil != err {
		log.Fatalf("Serve http error: %v", err)
		return
	}

	ph := &pacHandler{}
	if err := ph.reload(); nil != err {
		log.Fatal(err)
		return
	}

	go func(ls net.Listener) {
		listenAddr := ls.Addr().String()
		server := &http.Server{
			Addr: listenAddr,
		}
		server.Handler = ph
		if err := server.Serve(ls); !strings.Contains(err.Error(), "use of closed network connection") {
			log.Fatalf("HTTP server stop serve with error: %v", err)
		}
	}(ls)

	out := bytes.NewBuffer(nil)

	// Enable system proxy
	log.Print("Enable system proxy ...")
	if err := enableSystemProxy(fmt.Sprintf("http://%s/pac", ls.Addr().String())); nil != err {
		log.Fatalf("Enable system proxy error: %v", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Watch proxy config changed
	closed := false
	var closeMu sync.Mutex
	pacWatcher, err := fsnotify.NewWatcher()
	if nil != err {
		log.Fatalf("Can't watch pac file: %v", err)
		return
	}
	defer pacWatcher.Close()

	go func() {
		for {
			select {
			case event := <-pacWatcher.Events:
				{
					if event.Op&fsnotify.Write == fsnotify.Write {
						// Consume pendings write
						time.Sleep(time.Millisecond * 100)
						select {
						case <-pacWatcher.Events:
						default:
						}
						log.Printf("Pac file has changed, update pac config %s", event.String())
						closeMu.Lock()
						if closed {
							closeMu.Unlock()
							return
						}
						if err := ph.reload(); nil != err {
							log.Print(err)
							closeMu.Unlock()
							break
						}
						if err := enableSystemProxy(fmt.Sprintf("http://%s/pac?ts=%d", ls.Addr().String(), time.Now().Unix())); nil != err {
							log.Printf("Failed to update pac config: %v", err)
						} else {
							log.Printf("Pac update successfully")
						}
						closeMu.Unlock()
					}
				}
			case watchErr := <-pacWatcher.Errors:
				{
					log.Printf("Watch pac error: %v", watchErr)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	pacWatcher.Add(pacLocalPath)

	out.Reset()
	brookCh := make(chan error, 1)
	go func() {
		brookCmd := exec.CommandContext(ctx, flagBinPath, "client",
			"--ip", "127.0.0.1",
			"--listen", "127.0.0.1:1080",
			"--server", flagServer,
			"--password", flagPassword,
		)
		brookCmd.Stdout = out
		brookCmd.Stderr = out
		startErr := brookCmd.Start()
		if nil != startErr {
			brookCh <- err
			return
		}
		log.Printf("Brook client is start with address 127.0.0.1:1080")
		brookCh <- brookCmd.Wait()
	}()

	// Wait for signals to quit
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	select {
	case recvSig := <-sigCh:
		{
			log.Printf("Recv %v signal, shutting down ...", recvSig)
			cancel()
			<-brookCh
		}
	case brookErr := <-brookCh:
		{
			log.Printf("Brook occurs an error: %v (%v)", brookErr, out.String())
			cancel()
		}
	}
	closeMu.Lock()
	closed = true
	closeMu.Unlock()
	ls.Close()

	// Disable system proxy
	log.Print("Disable system proxy ...")
	out.Reset()
	brookCmd := exec.Command(flagBinPath, "systemproxy", "-r")
	brookCmd.Stdout = out
	brookCmd.Stderr = out
	if err := brookCmd.Start(); nil != err {
		log.Fatalf("Disable system proxy error: %v", err)
		return
	}
	if out.Len() != 0 {
		log.Fatalf("Disable system proxy error: %v", out.String())
		return
	}

	log.Printf("Bye ...")
}
