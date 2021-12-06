package logger

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	Trace, Info, Waring, Error *log.Logger
	configs                    = map[level]*loggerConfig{
		TRACE:   defaultConfig(TRACE),
		INFO:    defaultConfig(INFO),
		WARNING: defaultConfig(WARNING),
		ERROR:   defaultConfig(ERROR),
	}
	reg = regexp.MustCompile(`log\.(.+)\.((?i)out|format|prefix|reserve|filesuffix|compress)=(.+)`)
)

const (
	defaultFlag       = log.LstdFlags | log.Lshortfile
	defaultCompress   = true
	defaultReserve    = 0
	defaultTimeFormat = "20060102"
)

func init() {
	b, e := os.ReadFile("log.properties")
	if e != nil && !os.IsNotExist(e) {
		panic(e)
	}
	parseConfigs(b)
	for _, config := range configs {
		switch config.level {
		case TRACE:
			Trace = config.Create()
		case INFO:
			Info = config.Create()
		case WARNING:
			Waring = config.Create()
		case ERROR:
			Error = config.Create()
		}
	}
}

func parseConfigs(contents []byte) {
	lines := strings.Split(string(contents), "\n")
	for _, line := range lines {
		texts := strings.Split(line, "#")
		line = strings.TrimSpace(texts[0])
		if line == "" {
			continue
		}
		res := reg.FindStringSubmatch(line)
		if len(res) == 0 {
			continue
		}
		config, OK := configs[level(strings.ToUpper(res[1]))]
		if !OK {
			continue
		}
		switch strings.ToLower(res[2]) {
		case "out":
			if writers := parseOutWriter(strings.Split(res[3], ",")); len(writers) > 0 {
				config.out = writers
			}
		case "format":
			if flag, err := strconv.Atoi(res[3]); err == nil && flag < log.Lmsgprefix<<1 {
				config.flag = flag
			} else {
				fmt.Printf("Invalid format flag [%s],use default:[%d]\n", res[3], defaultFlag)
			}
		case "prefix":
			config.prefix = res[3]
		case "reserve":
			if reserve, err := strconv.Atoi(res[3]); err != nil {
				fmt.Printf("Invalid format reserve [%s],use default:[%d]\n", res[3], defaultReserve)
			} else if reserve > 0 {
				config.reserve = reserve
			}
		case "filesuffix":
			config.fileSuffix = res[3]
		case "compress":
			if compress, e := strconv.ParseBool(res[3]); e == nil {
				config.compress = compress
			} else {
				fmt.Printf("Invalid format compress [%s],use default:[%t]\n", res[3], defaultCompress)
			}
		default:
			fmt.Println("Invalid key :", res[2])
		}
	}
	return
}

func parseOutWriter(outs []string) []string {
	var writers []string
	for _, out := range outs {
		switch o := strings.ToLower(out); o {
		case "stdin", "stdout", "stderr", "discard":
			writers = append(writers, o)
		default:
			writers = append(writers, out)
		}
	}
	return writers
}

type loggerConfig struct {
	level              level
	out                []string
	prefix, fileSuffix string
	reserve, flag      int
	compress           bool
}

func (l *loggerConfig) Create() *log.Logger {
	ws := make([]io.Writer, 0)
	for _, o := range l.out {
		if w, OK := defaultWriter[o]; OK {
			ws = append(ws, w)
		} else {
			if l, e := newLogWriter(o, reserve(l.reserve), timeFormat(l.fileSuffix), compress(l.compress)); e == nil {
				ws = append(ws, l)
			} else {
				panic(e)
			}
		}
	}
	var out io.Writer
	if l := len(ws); l == 1 {
		out = ws[0]
	} else if l > 1 {
		out = io.MultiWriter(ws...)
	}
	if l.prefix != "" {
		l.prefix = fmt.Sprintf("[%s] ", l.prefix)
	}
	return log.New(out, l.prefix, l.flag)
}

var defaultWriter = map[string]io.Writer{
	"stdin":   os.Stdin,
	"stdout":  os.Stdout,
	"stderr":  os.Stderr,
	"discard": ioutil.Discard,
}

type level string

const (
	TRACE   level = "TRACE"
	INFO    level = "INFO"
	WARNING level = "WARNING"
	ERROR   level = "ERROR"
)

func defaultConfig(level level) *loggerConfig {
	return &loggerConfig{
		level:      level,
		out:        []string{"stdout"},
		prefix:     string(level),
		flag:       defaultFlag,
		compress:   defaultCompress,
		reserve:    defaultReserve,
		fileSuffix: defaultTimeFormat,
	}
}

//writer

const compressSuffix = ".gz"

type option func(*logWriter)

func reserve(day int) option {
	return func(l *logWriter) {
		l.reserve = day
	}
}

func compress(compressed bool) option {
	return func(l *logWriter) {
		l.compressed = compressed
	}
}

func timeFormat(format string) option {
	return func(l *logWriter) {
		l.timeFormat = format
	}
}

func newLogWriter(logPath string, options ...option) (*logWriter, error) {
	dir, name := filepath.Split(logPath)
	var err error
	if err := os.MkdirAll(dir, os.ModeDir|0744); err != nil {
		return nil, err
	}
	ext := filepath.Ext(name)
	l := &logWriter{
		dir:          dir,
		name:         strings.TrimSuffix(name, ext) + ".",
		ext:          ext,
		linkFileName: logPath,
	}
	for _, o := range options {
		o(l)
	}
	_, err = l.openOrNew()
	return l, err
}

type logWriter struct {
	dir, name, ext, suffix string
	linkFileName           string
	file                   *os.File

	reserve    int
	compressed bool
	timeFormat string
}

func (l *logWriter) Write(p []byte) (int, error) {
	f, err := l.openOrNew()
	if err != nil {
		fmt.Printf("write fail, msg(%s)\n", err)
		return 0, err
	}
	return f.Write(p)
}

func (l *logWriter) Close() error {
	if l.file == nil {
		return nil
	}
	defer func() { l.file = nil }()
	return l.file.Close()
}

func (l *logWriter) deleteFile() {
	if l.reserve <= 0 {
		return
	}
	minDate, _ := time.Parse(l.timeFormat, l.suffix)
	minDate = minDate.Add(time.Hour * time.Duration(-l.reserve*24))
	_ = filepath.Walk(l.dir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			fmt.Printf("open log dir %s failed", l.dir)
		}
		if info == nil {
			return nil
		}
		if info.IsDir() && l.dir != path {
			return fs.SkipDir
		}
		if info.IsDir() || path == l.linkFileName || path == l.fileName(l.suffix) {
			return nil
		}
		if t, e := l.timeFromName(info.Name()); e != nil {
			//fmt.Println(e)
		} else if t.Before(minDate) {
			if err = os.Remove(path); err != nil {
				fmt.Printf("remove file %s failed\n", path)
			}
		}
		return nil
	})
}

func (l *logWriter) timeFromName(filename string) (time.Time, error) {
	nameNoPrefix := strings.TrimPrefix(filename, l.name)
	if filename == nameNoPrefix {
		return time.Time{}, errors.New("mismatched prefix")
	}
	nameNoSuffix := strings.TrimSuffix(nameNoPrefix, compressSuffix)
	nameNoSuffix = strings.TrimSuffix(nameNoSuffix, l.ext)
	if nameNoPrefix == nameNoSuffix {
		return time.Time{}, errors.New("mismatched extension")
	}
	return time.Parse(l.timeFormat, nameNoSuffix)
}

func (l *logWriter) openOrNew() (*os.File, error) {
	suffix := l.timeSuffix()
	if l.file == nil || l.suffix != suffix {
		filename := l.fileName(suffix)
		_, err := os.Stat(filename)
		if err == nil && l.file == nil {
			l.file, err = os.OpenFile(filename, os.O_RDWR|os.O_APPEND, 0644)
		}
		if err == nil {
			return l.file, nil
		}
		if f, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644); err == nil {
			_ = l.compress()
			l.file = f
			l.suffix = suffix
			go l.deleteFile()
			if err = os.Remove(l.linkFileName); err == nil || os.IsNotExist(err) {
				err = os.Link(filename, l.linkFileName)
			}
			if err != nil {
				fmt.Println("rotate log file error:", err)
			}
		} else {
			if l.file == nil {
				return nil, fmt.Errorf("can't open new logfile: %s", err)
			} else {
				fmt.Println("can't open new logfile: ", err)
				return f, nil
			}
		}
	}
	return l.file, nil
}

func (l *logWriter) compress() (err error) {
	defer l.file.Close()
	if l.file == nil || !l.compressed {
		return nil
	}
	fi, err := l.file.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat log file: %v", err)
	}
	src := l.file.Name()
	dst := src + compressSuffix
	gzf, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fi.Mode())
	if err != nil {
		return fmt.Errorf("failed to open compressed log file: %v", err)
	}
	defer gzf.Close()
	gz := gzip.NewWriter(gzf)
	defer func() {
		if err != nil {
			_ = os.Remove(dst)
			err = fmt.Errorf("failed to compress log file: %v", err)
		}
	}()
	if _, err = l.file.Seek(0, 0); err == nil {
		if _, err = io.Copy(gz, l.file); err == nil {
			if err = gz.Close(); err == nil {
				if err = gzf.Close(); err == nil {
					if err = l.file.Close(); err == nil {
						err = os.Remove(src)
					}
				}
			}
		}
	}
	return err
}

func (l *logWriter) fileName(suffix string) string {
	return filepath.Join(l.dir, fmt.Sprintf("%s%s%s", l.name, suffix, l.ext))
}

func (l *logWriter) timeSuffix() string {
	return time.Now().Format(l.timeFormat)
}
