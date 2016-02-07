/*
Executing shell commands via simple http server.
Settings through 2 command line arguments, path and shell command.
By default bind to :8080.

Install/update:
	go get -u github.com/msoap/shell2http
	ln -s $GOPATH/bin/shell2http ~/bin/shell2http

MacOS install:
	brew tap msoap/tools
	brew install shell2http
	# update:
	brew update; brew upgrade shell2http

Usage:
	shell2http [options] /path "shell command" /path2 "shell command2" ...
	options:
		-host="host"    : host for http server, default - all interfaces
		-port=NNNN      : port for http server, default - 8080
		-form           : parse query into environment vars
		-cgi            : exec as CGI-script
		                  set environment variables
		                  write POST-data to STDIN (if not set -form)
		                  parse headers from script (Location: XXX)
		-export-vars=var: export environment vars ("VAR1,VAR2,...")
		-export-all-vars: export all current environment vars
		-no-index       : dont generate index page
		-add-exit       : add /exit command
		-log=filename   : log filename, default - STDOUT
		-shell="shell"  : shell for execute command, "" - without shell
		-cache=NNN      : caching command out for NNN seconds
		-one-thread     : run each shell command in one thread
		-version
		-help

Examples:
	shell2http /top "top -l 1 | head -10"
	shell2http /date date /ps "ps aux"
	shell2http -export-all-vars /env 'printenv | sort' /env/path 'echo $PATH' /env/gopath 'echo $GOPATH'
	shell2http -export-all-vars /shell_vars_json 'perl -MJSON -E "say to_json(\%ENV)"'
	shell2http /cal_html 'echo "<html><body><h1>Calendar</h1>Date: <b>$(date)</b><br><pre>$(cal $(date +%Y))</pre></body></html>"'
	shell2http -form /form 'echo $v_from, $v_to'
	shell2http -cgi /user_agent 'echo $HTTP_USER_AGENT'
	shell2http -cgi /set 'touch file; echo "Location: /\n"'
	shell2http -export-vars=GOPATH /get 'echo $GOPATH'

More complex examples:

simple http-proxy server (for logging all URLs)
	# setup proxy as "http://localhost:8080/"
	shell2http \
		-log=/dev/null \
		-cgi \
		/ 'echo $REQUEST_URI 1>&2; [ "$REQUEST_METHOD" == "POST" ] && post_param="-d@-"; curl -sL $post_param "$REQUEST_URI" -A "$HTTP_USER_AGENT"'

test slow connection
	# http://localhost:8080/slow?duration=10
	shell2http -form /slow 'sleep ${v_duration:-1}; echo "sleep ${v_duration:-1} seconds"'

proxy with cache in files (for debug with production API with rate limit)
	# get "http://localhost:8080/get?url=http://api.url/"
	shell2http \
		-form \
		/form 'echo "<html><form action=/get>URL: <input name=url><input type=submit>"' \
		/get 'MD5=$(printf "%s" $v_url | md5); cat cache_$MD5 || (curl -sL $v_url | tee cache_$MD5)'

remote sound volume control (Mac OS)
	shell2http \
		/get  'osascript -e "output volume of (get volume settings)"' \
		/up   'osascript -e "set volume output volume (($(osascript -e "output volume of (get volume settings)")+10))"' \
		/down 'osascript -e "set volume output volume (($(osascript -e "output volume of (get volume settings)")-10))"'

remote control for Vox.app player (Mac OS)
	shell2http \
		/play_pause 'osascript -e "tell application \"Vox\" to playpause" && echo ok' \
		/get_info 'osascript -e "tell application \"Vox\"" -e "\"Artist: \" & artist & \"\n\" & \"Album: \" & album & \"\n\" & \"Track: \" & track" -e "end tell"'

get four random OS X wallpapers
	shell2http \
		/img 'cat "$(ls "/Library/Desktop Pictures/"*.jpg | ruby -e "puts STDIN.readlines.shuffle[0]")"' \
		/wallpapers 'echo "<html><h3>OS X Wallpapers</h3>"; seq 4 | xargs -I@ echo "<img src=/img?@ width=500>"'

More examples on https://github.com/msoap/shell2http/wiki

*/
package main

import (
	"flag"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/koding/cache"
)

// VERSION - version
const VERSION = "1.4"

// PORT - default port for http-server
const PORT = 8080

// ------------------------------------------------------------------

// INDEX_HTML - Template for index page
const INDEX_HTML = `<!DOCTYPE html>
<html>
<head>
	<title>shell2http</title>
</head>
<body>
	<h1>shell2http</h1>
	<ul>
		%s
	</ul>
	Get from: <a href="https://github.com/msoap/shell2http">github.com/msoap/shell2http</a>
</body>
</html>
`

// ------------------------------------------------------------------

// Command - one command type
type Command struct {
	path string
	cmd  string
}

// Config - config struct
type Config struct {
	host          string // server host
	port          int    // server port
	setCGI        bool   // set CGI variables
	setForm       bool   // parse form from URL
	noIndex       bool   // dont generate index page
	addExit       bool   // add /exit command
	exportVars    string // list of environment vars for export to script
	exportAllVars bool   // export all current environment vars
	shell         string // export all current environment vars
	cache         int    // caching command out (in seconds)
	oneThread     bool   // run each shell commands in one thread
}

// ------------------------------------------------------------------
// parse arguments
func getConfig() (cmd_handlers []Command, app_config Config, err error) {
	var log_filename string
	flag.StringVar(&log_filename, "log", "", "log filename, default - STDOUT")
	flag.IntVar(&app_config.port, "port", PORT, "port for http server")
	flag.StringVar(&app_config.host, "host", "", "host for http server")
	flag.BoolVar(&app_config.setCGI, "cgi", false, "exec as CGI-script")
	flag.StringVar(&app_config.exportVars, "export-vars", "", "export environment vars (\"VAR1,VAR2,...\")")
	flag.BoolVar(&app_config.exportAllVars, "export-all-vars", false, "export all current environment vars")
	flag.BoolVar(&app_config.setForm, "form", false, "parse query into environment vars")
	flag.BoolVar(&app_config.noIndex, "no-index", false, "dont generate index page")
	flag.BoolVar(&app_config.addExit, "add-exit", false, "add /exit command")
	flag.StringVar(&app_config.shell, "shell", "sh", "custom shell or \"\" for execute without shell")
	flag.IntVar(&app_config.cache, "cache", 0, "caching command out (in seconds)")
	flag.BoolVar(&app_config.oneThread, "one-thread", false, "run each shell command in one thread")
	flag.Usage = func() {
		fmt.Printf("usage: %s [options] /path \"shell command\" /path2 \"shell command2\"\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(0)
	}
	version := flag.Bool("version", false, "get version")
	flag.Parse()
	if *version {
		fmt.Println(VERSION)
		os.Exit(0)
	}

	// setup log file
	if len(log_filename) > 0 {
		fh_log, err := os.OpenFile(log_filename, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("error opening log file: %v", err)
		}
		log.SetOutput(fh_log)
	}

	// need >= 2 arguments and count of it must be even
	args := flag.Args()
	if len(args) < 2 || len(args)%2 == 1 {
		return nil, Config{}, fmt.Errorf("error: need pairs of path and shell command")
	}

	for i := 0; i < len(args); i += 2 {
		path, cmd := args[i], args[i+1]
		if path[0] != '/' {
			return nil, Config{}, fmt.Errorf("error: path %s dont starts with /", path)
		}
		cmd_handlers = append(cmd_handlers, Command{path: path, cmd: cmd})
	}

	return cmd_handlers, app_config, nil
}

// ------------------------------------------------------------------
// setup http handlers
func setupHandlers(cmd_handlers []Command, app_config Config, cacheTTL *cache.MemoryTTL) {
	index_li_html := ""
	exists_root_path := false

	for _, row := range cmd_handlers {
		path, cmd := row.path, row.cmd
		mutex := sync.Mutex{}

		shell_handler := func(rw http.ResponseWriter, req *http.Request) {
			log.Println(req.Method, path)

			setCommonHeaders(rw)

			if app_config.cache > 0 {
				cacheData, err := cacheTTL.Get(path)
				if err != cache.ErrNotFound && err != nil {
					log.Print(err)
				} else if err == nil {
					// cache hit
					fmt.Fprint(rw, cacheData.(string))
					return
				}
			}

			shell, params := "sh", []string{"-c", cmd}
			if runtime.GOOS == "windows" {
				shell, params = "cmd", []string{"/C", cmd}
			}

			// custom shell
			if app_config.shell != "sh" {
				if app_config.shell != "" {
					shell = app_config.shell
				} else {
					cmd_line := regexp.MustCompile(`\s+`).Split(cmd, -1)
					shell, params = cmd_line[0], cmd_line[1:]
				}
			}

			os_exec_command := exec.Command(shell, params...)

			proxySystemEnv(os_exec_command, app_config)
			if app_config.setForm {
				getForm(os_exec_command, req)
			}

			if app_config.setCGI {
				setCGIEnv(os_exec_command, req, app_config)
			}

			if app_config.oneThread {
				mutex.Lock()
				defer mutex.Unlock()
			}

			os_exec_command.Stderr = os.Stderr
			shell_out, err := os_exec_command.Output()

			if err != nil {
				log.Println("exec error: ", err)
				fmt.Fprint(rw, "exec error: ", err)
			} else {
				out_text := string(shell_out)
				if app_config.setCGI {
					headers := map[string]string{}
					out_text, headers = parseCGIHeaders(out_text)
					for header_key, header_value := range headers {
						rw.Header().Set(header_key, header_value)
						if header_key == "Location" {
							rw.WriteHeader(http.StatusFound)
						}
					}
				}
				fmt.Fprint(rw, out_text)

				if app_config.cache > 0 {
					err := cacheTTL.Set(path, out_text)
					if err != nil {
						log.Print(err)
					}
				}
			}

			return
		}

		http.HandleFunc(path, shell_handler)
		exists_root_path = exists_root_path || path == "/"

		log.Printf("register: %s (%s)\n", path, cmd)
		index_li_html += fmt.Sprintf(`<li><a href="%s">%s</a> <span style="color: #888">- %s<span></li>`, path, path, html.EscapeString(cmd))
	}

	// --------------
	if app_config.addExit {
		http.HandleFunc("/exit", func(rw http.ResponseWriter, req *http.Request) {
			log.Println("GET /exit")
			setCommonHeaders(rw)
			fmt.Fprint(rw, "Bye...")
			go os.Exit(0)

			return
		})

		log.Printf("register: %s (%s)\n", "/exit", "/exit")
		index_li_html += fmt.Sprintf(`<li><a href="%s">%s</a></li>`, "/exit", "/exit")
	}

	// --------------
	if !app_config.noIndex && !exists_root_path {
		index_html := fmt.Sprintf(INDEX_HTML, index_li_html)
		http.HandleFunc("/", func(rw http.ResponseWriter, req *http.Request) {
			setCommonHeaders(rw)
			if req.URL.Path != "/" {
				log.Println("404")
				http.NotFound(rw, req)
				return
			}
			log.Println("GET /")
			fmt.Fprint(rw, index_html)

			return
		})
	}
}

// ------------------------------------------------------------------
// set some CGI variables
func setCGIEnv(cmd *exec.Cmd, req *http.Request, app_config Config) {
	// set HTTP_* variables
	for header_name, header_value := range req.Header {
		env_name := strings.ToUpper(strings.Replace(header_name, "-", "_", -1))
		cmd.Env = append(cmd.Env, fmt.Sprintf("HTTP_%s=%s", env_name, header_value[0]))
	}

	remote_addr := strings.Split(req.RemoteAddr, ":")
	CGI_vars := [...]struct {
		cgi_name, value string
	}{
		{"PATH_INFO", req.URL.Path},
		{"QUERY_STRING", req.URL.RawQuery},
		{"REMOTE_ADDR", remote_addr[0]},
		{"REMOTE_PORT", remote_addr[1]},
		{"REQUEST_METHOD", req.Method},
		{"REQUEST_URI", req.RequestURI},
		{"SCRIPT_NAME", req.URL.Path},
		{"SERVER_NAME", app_config.host},
		{"SERVER_PORT", fmt.Sprintf("%d", app_config.port)},
		{"SERVER_PROTOCOL", req.Proto},
		{"SERVER_SOFTWARE", "shell2http"},
	}

	for _, row := range CGI_vars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", row.cgi_name, row.value))
	}

	// get POST data to stdin of script (if not parse form vars above)
	if req.Method == "POST" && !app_config.setForm {

		stdin, err := cmd.StdinPipe()
		if err != nil {
			log.Println("get STDIN error: ", err)
			return
		}

		post_body, err := ioutil.ReadAll(req.Body)
		if err != nil {
			log.Println("read POST data error: ", err)
			return
		}

		io.WriteString(stdin, string(post_body))
		stdin.Close()
	}
}

// ------------------------------------------------------------------
/* parse headers from script output:

Header-name1: value1\n
Header-name2: value2\n
\n
text

*/
func parseCGIHeaders(shell_out string) (string, map[string]string) {
	headers_map := map[string]string{}
	parts := regexp.MustCompile(`\r?\n\r?\n`).Split(shell_out, 2)
	if len(parts) == 2 {
		re := regexp.MustCompile(`(\S+):\s*(.+)\r?\n?`)
		headers := re.FindAllStringSubmatch(parts[0], -1)
		if len(headers) > 0 {
			for _, header := range headers {
				headers_map[header[1]] = header[2]
			}
			return parts[1], headers_map
		}
	}

	// headers dont found, return all text
	return shell_out, headers_map
}

// ------------------------------------------------------------------
// parse form into environment vars
func getForm(cmd *exec.Cmd, req *http.Request) {
	err := req.ParseForm()
	if err != nil {
		log.Println(err)
		return
	}

	for key, value := range req.Form {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", "v_"+key, strings.Join(value, ",")))
	}
}

// ------------------------------------------------------------------
// proxy some system vars
func proxySystemEnv(cmd *exec.Cmd, app_config Config) {
	vars_names := []string{"PATH", "HOME", "LANG", "USER", "TMPDIR"}

	if app_config.exportVars != "" {
		vars_names = append(vars_names, strings.Split(app_config.exportVars, ",")...)
	}

	for _, env_raw := range os.Environ() {
		env := strings.SplitN(env_raw, "=", 2)
		if app_config.exportAllVars {
			cmd.Env = append(cmd.Env, env_raw)
		} else {
			for _, env_var_name := range vars_names {
				if env[0] == env_var_name {
					cmd.Env = append(cmd.Env, env_raw)
				}
			}
		}
	}
}

// ------------------------------------------------------------------
// set headers for all handlers
func setCommonHeaders(rw http.ResponseWriter) {
	rw.Header().Set("Server", fmt.Sprintf("shell2http %s", VERSION))
}

// ------------------------------------------------------------------
func main() {
	cmd_handlers, app_config, err := getConfig()
	if err != nil {
		log.Fatal(err)
	}

	var cacheTTL *cache.MemoryTTL
	if app_config.cache > 0 {
		cacheTTL = cache.NewMemoryWithTTL(time.Duration(app_config.cache) * time.Second)
		cacheTTL.StartGC(time.Duration(app_config.cache) * time.Second * 2)
	}
	setupHandlers(cmd_handlers, app_config, cacheTTL)

	adress := fmt.Sprintf("%s:%d", app_config.host, app_config.port)
	log.Printf("listen http://%s/\n", adress)
	err = http.ListenAndServe(adress, nil)
	if err != nil {
		log.Fatal(err)
	}
}
