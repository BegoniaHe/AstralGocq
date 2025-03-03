package global

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/mattn/go-colorable"
	"github.com/sirupsen/logrus"
)

// 日志颜色常量
const (
	colorCodePanic = "\x1b[1;31m" // 红色加粗
	colorCodeFatal = "\x1b[1;31m" // 红色加粗
	colorCodeError = "\x1b[31m"   // 红色
	colorCodeWarn  = "\x1b[33m"   // 黄色
	colorCodeInfo  = "\x1b[37m"   // 白色
	colorCodeDebug = "\x1b[32m"   // 绿色
	colorCodeTrace = "\x1b[36m"   // 青色
	colorReset     = "\x1b[0m"    // 重置颜色
)

// LogFormat 专用于 go-cqhttp 的日志格式
type LogFormat struct {
	EnableColor bool
}

// Format 实现 logrus.Formatter 接口
func (f LogFormat) Format(entry *logrus.Entry) ([]byte, error) {
	buf := NewBuffer()
	defer PutBuffer(buf)

	if f.EnableColor {
		buf.WriteString(GetLogLevelColorCode(entry.Level))
	}

	buf.WriteByte('[')
	buf.WriteString(entry.Time.Format("2006-01-02 15:04:05"))
	buf.WriteString("] [")
	buf.WriteString(strings.ToUpper(entry.Level.String()))
	buf.WriteString("]: ")
	buf.WriteString(entry.Message)
	buf.WriteString(" \n")

	if f.EnableColor {
		buf.WriteString(colorReset)
	}

	ret := make([]byte, len(buf.Bytes()))
	copy(ret, buf.Bytes()) // 复制缓冲区
	return ret, nil
}

// LocalHook logrus本地钩子
type LocalHook struct {
	lock      *sync.Mutex
	levels    []logrus.Level   // hook级别
	formatter logrus.Formatter // 格式
	path      string           // 写入path
	writer    io.Writer        // io
}

// NewLocalHook 初始化本地日志钩子实现
func NewLocalHook(args any, consoleFormatter, fileFormatter logrus.Formatter, levels ...logrus.Level) *LocalHook {
	hook := &LocalHook{
		lock:   new(sync.Mutex),
		levels: levels,
	}
	hook.SetFormatter(consoleFormatter, fileFormatter)

	switch arg := args.(type) {
	case string:
		hook.SetPath(arg)
	case io.Writer:
		hook.SetWriter(arg)
	default:
		panic(fmt.Sprintf("不支持的类型: %v", reflect.TypeOf(args)))
	}

	return hook
}

// Levels 实现 logrus Hook 接口，返回钩子支持的日志级别
func (hook *LocalHook) Levels() []logrus.Level {
	if len(hook.levels) == 0 {
		return logrus.AllLevels
	}
	return hook.levels
}

// Fire 实现 logrus Hook 接口，处理日志条目
func (hook *LocalHook) Fire(entry *logrus.Entry) error {
	hook.lock.Lock()
	defer hook.lock.Unlock()

	if hook.writer != nil {
		return hook.ioWrite(entry)
	}

	if hook.path != "" {
		return hook.pathWrite(entry)
	}

	return nil
}

// 写入到 io.Writer
func (hook *LocalHook) ioWrite(entry *logrus.Entry) error {
	log, err := hook.formatter.Format(entry)
	if err != nil {
		return err
	}

	_, err = hook.writer.Write(log)
	return err
}

// 写入到文件
func (hook *LocalHook) pathWrite(entry *logrus.Entry) error {
	// 确保目录存在
	dir := filepath.Dir(hook.path)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return err
	}

	// 打开文件
	fd, err := os.OpenFile(hook.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	defer fd.Close()

	// 格式化并写入日志
	log, err := hook.formatter.Format(entry)
	if err != nil {
		return err
	}

	_, err = fd.Write(log)
	return err
}

// SetFormatter 设置日志格式
func (hook *LocalHook) SetFormatter(consoleFormatter, fileFormatter logrus.Formatter) {
	hook.lock.Lock()
	defer hook.lock.Unlock()

	// 支持处理windows平台的console色彩
	logrus.SetOutput(colorable.NewColorableStdout())
	// 用于在console写出
	logrus.SetFormatter(consoleFormatter)
	// 用于写入文件
	hook.formatter = fileFormatter
}

// SetWriter 设置Writer
func (hook *LocalHook) SetWriter(writer io.Writer) {
	hook.lock.Lock()
	defer hook.lock.Unlock()
	hook.writer = writer
}

// SetPath 设置日志写入路径
func (hook *LocalHook) SetPath(path string) {
	hook.lock.Lock()
	defer hook.lock.Unlock()
	hook.path = path
}

// GetLogLevel 获取日志等级
//
// 可能的值有
// "trace", "debug", "info", "warn", "error"
func GetLogLevel(level string) []logrus.Level {
	switch strings.ToLower(level) {
	case "trace":
		return []logrus.Level{
			logrus.TraceLevel, logrus.DebugLevel,
			logrus.InfoLevel, logrus.WarnLevel, logrus.ErrorLevel,
			logrus.FatalLevel, logrus.PanicLevel,
		}
	case "debug":
		return []logrus.Level{
			logrus.DebugLevel, logrus.InfoLevel,
			logrus.WarnLevel, logrus.ErrorLevel,
			logrus.FatalLevel, logrus.PanicLevel,
		}
	case "info":
		return []logrus.Level{
			logrus.InfoLevel, logrus.WarnLevel,
			logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel,
		}
	case "warn":
		return []logrus.Level{
			logrus.WarnLevel, logrus.ErrorLevel,
			logrus.FatalLevel, logrus.PanicLevel,
		}
	case "error":
		return []logrus.Level{
			logrus.ErrorLevel, logrus.FatalLevel,
			logrus.PanicLevel,
		}
	default:
		return []logrus.Level{
			logrus.InfoLevel, logrus.WarnLevel,
			logrus.ErrorLevel, logrus.FatalLevel, logrus.PanicLevel,
		}
	}
}

// GetLogLevelColorCode 获取日志等级对应色彩code
func GetLogLevelColorCode(level logrus.Level) string {
	switch level {
	case logrus.PanicLevel:
		return colorCodePanic
	case logrus.FatalLevel:
		return colorCodeFatal
	case logrus.ErrorLevel:
		return colorCodeError
	case logrus.WarnLevel:
		return colorCodeWarn
	case logrus.InfoLevel:
		return colorCodeInfo
	case logrus.DebugLevel:
		return colorCodeDebug
	case logrus.TraceLevel:
		return colorCodeTrace
	default:
		return colorCodeInfo
	}
}
