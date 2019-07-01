package main

// This command line utility builds on top of the p4d log analyzer
// and outputs Prometheus metrics in a single file to be picked up by
// node_exporter's textfile.collector module.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"time"

	p4dlog "github.com/rcowham/go-libp4dlog"
	"github.com/rcowham/go-libp4dlog/cmd/p4prometheus/config"
	"github.com/rcowham/go-libtail/tailer"
	"github.com/rcowham/go-libtail/tailer/fswatcher"
	"github.com/rcowham/go-libtail/tailer/glob"

	"github.com/sirupsen/logrus"
)

var blankTime time.Time

var (
	configfile = flag.String("config", "p4prometheus.yaml", "Config file.")
	debug      = flag.Bool("debug", false, "debug level")
)

// Structure for use with libtail
type logConfig struct {
	Type                 string
	Path                 string
	PollInterval         time.Duration
	Readall              bool
	FailOnMissingLogfile bool
}

// P4Prometheus structure
type P4Prometheus struct {
	config            *config.Config
	logger            *logrus.Logger
	cmdCounter        map[string]int32
	cmdCumulative     map[string]float64
	totalReadWait     map[string]float64
	totalReadHeld     map[string]float64
	totalWriteWait    map[string]float64
	totalWriteHeld    map[string]float64
	totalTriggerLapse map[string]float64
	lines             chan []byte
	events            chan string
	lastOutputTime    time.Time
}

func newP4Prometheus(config *config.Config, logger *logrus.Logger) (p4p *P4Prometheus) {
	return &P4Prometheus{
		config:            config,
		logger:            logger,
		lines:             make(chan []byte, 10000),
		events:            make(chan string, 10000),
		cmdCounter:        make(map[string]int32),
		cmdCumulative:     make(map[string]float64),
		totalReadWait:     make(map[string]float64),
		totalReadHeld:     make(map[string]float64),
		totalWriteWait:    make(map[string]float64),
		totalWriteHeld:    make(map[string]float64),
		totalTriggerLapse: make(map[string]float64),
	}
}

func (p4p *P4Prometheus) publishCumulative() {
	if p4p.lastOutputTime == blankTime || time.Now().Sub(p4p.lastOutputTime) >= time.Second*10 {
		f, err := os.Create(p4p.config.MetricsOutput)
		if err != nil {
			p4p.logger.Errorf("Error opening %s: %v", p4p.config.MetricsOutput, err)
			return
		}
		p4p.logger.Infof("Writing stats\n")
		p4p.lastOutputTime = time.Now()
		fmt.Fprintf(f, "# HELP p4_cmd_counter A count of completed p4 cmds (by cmd)\n"+
			"# TYPE p4_cmd_counter counter\n")
		for cmd, count := range p4p.cmdCounter {
			buf := fmt.Sprintf("p4_cmd_counter{cmd=\"%s\",serverid=\"%s\",sdpinst=\"%s\"} %d\n",
				cmd, p4p.config.ServerID, p4p.config.SDPInstance, count)
			p4p.logger.Debugf(buf)
			fmt.Fprint(f, buf)
		}
		fmt.Fprintf(f, "# HELP p4_cmd_cumulative_seconds The total in seconds (by cmd)\n"+
			"# TYPE p4_cmd_cumulative_seconds counter\n")
		for cmd, lapse := range p4p.cmdCumulative {
			buf := fmt.Sprintf("p4_cmd_cumulative_seconds{cmd=\"%s\",serverid=\"%s\",sdpinst=\"%s\"} %0.3f\n",
				cmd, p4p.config.ServerID, p4p.config.SDPInstance, lapse)
			p4p.logger.Debugf(buf)
			fmt.Fprint(f, buf)
		}
		fmt.Fprintf(f, "# HELP p4_total_read_wait_seconds The total waiting for read locks in seconds (by table)\n"+
			"# TYPE p4_total_read_wait_seconds counter\n")
		for table, total := range p4p.totalReadWait {
			buf := fmt.Sprintf("p4_total_read_wait_seconds{table=\"%s\",serverid=\"%s\",sdpinst=\"%s\"} %0.3f\n",
				table, p4p.config.ServerID, p4p.config.SDPInstance, total)
			p4p.logger.Debugf(buf)
			fmt.Fprint(f, buf)
		}
		fmt.Fprintf(f, "# HELP p4_total_read_held_seconds The total read locks held in seconds (by table)\n"+
			"# TYPE p4_total_read_held_seconds counter\n")
		for table, total := range p4p.totalReadHeld {
			buf := fmt.Sprintf("p4_total_read_held_seconds{table=\"%s\",serverid=\"%s\",sdpinst=\"%s\"} %0.3f\n",
				table, p4p.config.ServerID, p4p.config.SDPInstance, total)
			p4p.logger.Debugf(buf)
			fmt.Fprint(f, buf)
		}
		fmt.Fprintf(f, "# HELP p4_total_write_wait_seconds The total waiting for write locks in seconds (by table)\n"+
			"# TYPE p4_total_write_wait_seconds counter\n")
		for table, total := range p4p.totalWriteWait {
			buf := fmt.Sprintf("p4_total_write_wait_seconds{table=\"%s\",serverid=\"%s\",sdpinst=\"%s\"} %0.3f\n",
				table, p4p.config.ServerID, p4p.config.SDPInstance, total)
			p4p.logger.Debugf(buf)
			fmt.Fprint(f, buf)
		}
		fmt.Fprintf(f, "# HELP p4_total_write_held_seconds The total write locks held in seconds (by table)\n"+
			"# TYPE p4_total_write_held_seconds counter\n")
		for table, total := range p4p.totalWriteHeld {
			buf := fmt.Sprintf("p4_total_write_held_seconds{table=\"%s\",serverid=\"%s\",sdpinst=\"%s\"} %0.3f\n",
				table, p4p.config.ServerID, p4p.config.SDPInstance, total)
			p4p.logger.Debugf(buf)
			fmt.Fprint(f, buf)
		}
		fmt.Fprintf(f, "# HELP p4_total_trigger_lapse_seconds The total lapse time for triggers in seconds (by trigger)\n"+
			"# TYPE p4_total_trigger_lapse_seconds counter\n")
		for table, total := range p4p.totalTriggerLapse {
			buf := fmt.Sprintf("p4_total_trigger_lapse_seconds{trigger=\"%s\",serverid=\"%s\",sdpinst=\"%s\"} %0.3f\n",
				table, p4p.config.ServerID, p4p.config.SDPInstance, total)
			p4p.logger.Debugf(buf)
			fmt.Fprint(f, buf)
		}
		err = f.Close()
		if err != nil {
			p4p.logger.Errorf("Error closing file: %v", err)
		}
		err = os.Chmod(p4p.config.MetricsOutput, 0644)
		if err != nil {
			p4p.logger.Errorf("Error chmod-ing file: %v", err)
		}
	}
}

func (p4p *P4Prometheus) getSeconds(tmap map[string]interface{}, fieldName string) float64 {
	p4p.logger.Debugf("field %s %v, %v\n", fieldName, reflect.TypeOf(tmap[fieldName]), tmap[fieldName])
	if total, ok := tmap[fieldName].(float64); ok {
		return (total)
	}
	return 0
}

func (p4p *P4Prometheus) getMilliseconds(tmap map[string]interface{}, fieldName string) float64 {
	p4p.logger.Debugf("field %s %v, %v\n", fieldName, reflect.TypeOf(tmap[fieldName]), tmap[fieldName])
	if total, ok := tmap[fieldName].(float64); ok {
		return (total / 1000)
	}
	return 0
}

func (p4p *P4Prometheus) publishEvent(str string) {
	p4p.logger.Debugf("publish json: %v\n", str)
	var f interface{}
	err := json.Unmarshal([]byte(str), &f)
	if err != nil {
		fmt.Printf("Error %v to unmarshal %s", err, str)
	}
	m := f.(map[string]interface{})
	p4p.logger.Debugf("unmarshalled: %v\n", m)
	p4p.logger.Debugf("cmd{\"%s\"}=%0.3f\n", m["cmd"], m["completedLapse"])
	var cmd string
	var ok bool
	if cmd, ok = m["cmd"].(string); !ok {
		fmt.Printf("Failed string: %v", m["cmd"])
		return
	}
	var lapse float64
	if lapse, ok = m["completedLapse"].(float64); !ok {
		fmt.Printf("Failed float: %v", m["completedLapse"])
		return
	}

	p4p.cmdCounter[cmd]++
	p4p.cmdCumulative[cmd] += lapse

	var tables []interface{}
	const triggerPrefix = "trigger_"
	p4p.logger.Debugf("Type: %v\n", reflect.TypeOf(m["tables"]))
	if tables, ok = m["tables"].([]interface{}); ok {
		for i := range tables {
			p4p.logger.Debugf("table: %v, %v\n", reflect.TypeOf(tables[i]), tables[i])
			var table map[string]interface{}
			var tableName string
			if table, ok = tables[i].(map[string]interface{}); !ok {
				continue
			}
			if tableName, ok = table["tableName"].(string); !ok {
				continue
			}
			if tableName == "" {
				continue
			}
			if len(tableName) > len(triggerPrefix) && tableName[:len(triggerPrefix)] == triggerPrefix {
				triggerName := tableName[len(triggerPrefix):]
				p4p.totalTriggerLapse[triggerName] += p4p.getSeconds(table, "triggerLapse")
			} else {
				p4p.totalReadHeld[tableName] += p4p.getMilliseconds(table, "totalReadHeld")
				p4p.totalReadWait[tableName] += p4p.getMilliseconds(table, "totalReadWait")
				p4p.totalWriteHeld[tableName] += p4p.getMilliseconds(table, "totalWriteHeld")
				p4p.totalWriteWait[tableName] += p4p.getMilliseconds(table, "totalWriteWait")
			}
		}
	}
	p4p.publishCumulative()
}

func (p4p *P4Prometheus) processEvents() {
	for {
		select {
		case json := <-p4p.events:
			p4p.publishEvent(json)
		default:
			return
		}
	}
}

func startTailer(cfgInput *logConfig, logger *logrus.Logger) (fswatcher.FileTailer, error) {

	var tail fswatcher.FileTailer
	g, err := glob.FromPath(cfgInput.Path)
	if err != nil {
		return nil, err
	}
	switch {
	case cfgInput.Type == "file":
		if cfgInput.PollInterval == 0 {
			tail, err = fswatcher.RunFileTailer([]glob.Glob{g}, cfgInput.Readall, cfgInput.FailOnMissingLogfile, logger)
		} else {
			tail, err = fswatcher.RunPollingFileTailer([]glob.Glob{g}, cfgInput.Readall, cfgInput.FailOnMissingLogfile, cfgInput.PollInterval, logger)
		}
	case cfgInput.Type == "stdin":
		tail = tailer.RunStdinTailer()
	default:
		return nil, fmt.Errorf("config error: Input type '%v' unknown", cfgInput.Type)
	}
	return tail, nil
}

func exitOnError(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err.Error())
		os.Exit(-1)
	}
}

func readServerID(logger *logrus.Logger, instance string) string {
	idfile := fmt.Sprintf("/p4/%s/root/server.id", instance)
	if _, err := os.Stat(idfile); err == nil {
		buf, err := ioutil.ReadFile(idfile) // just pass the file name
		if err != nil {
			logger.Errorf("Failed to read %v - %v", idfile, err)
			return ""
		}
		return string(buf)
	}
	return ""
}

func main() {
	flag.Parse()

	logger := logrus.New()
	logger.Level = logrus.InfoLevel
	if *debug {
		logger.Level = logrus.DebugLevel
	}

	cfg, err := config.LoadConfigFile(*configfile)
	if err != nil {
		exitOnError(err)
	}
	logger.Infof("Processing log file: '%s' output to '%s' SDP instance '%s'\n",
		cfg.LogPath, cfg.MetricsOutput, cfg.SDPInstance)
	if len(cfg.ServerID) == 0 {
		cfg.ServerID = readServerID(logger, cfg.SDPInstance)
	}
	logger.Infof("Server id: '%s'\n", cfg.ServerID)
	p4p := newP4Prometheus(cfg, logger)

	fp := p4dlog.NewP4dFileParser()
	go fp.LogParser(p4p.lines, p4p.events)

	//---------------

	logcfg := &logConfig{
		Type:                 "file",
		Path:                 cfg.LogPath,
		PollInterval:         0,
		Readall:              true,
		FailOnMissingLogfile: true,
	}

	tail, err := startTailer(logcfg, logger)
	exitOnError(err)

	lineNo := 0
	for {
		select {
		case <-time.After(time.Second * 1):
			p4p.processEvents()
			p4p.publishCumulative()
		case err := <-tail.Errors():
			if os.IsNotExist(err.Cause()) {
				exitOnError(fmt.Errorf("error reading log lines: %v: use 'fail_on_missing_logfile: false' in the input configuration if you want grok_exporter to start even though the logfile is missing", err))
			} else {
				exitOnError(fmt.Errorf("error reading log lines: %v", err.Error()))
			}
		case line := <-tail.Lines():
			lineNo++
			p4p.lines <- []byte(line.Line)
		case json := <-p4p.events:
			p4p.publishEvent(json)
		}
	}

	// sigs := make(chan os.Signal, 1)
	// done := make(chan bool, 1)

	// // registers to receive notifications of the specified signals.
	// signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// // Block waiting for signals. When it gets one it'll print it out
	// // and then notify the program that it can finish.
	// go func() {
	// 	sig := <-sigs
	// 	fmt.Println()
	// 	fmt.Println(sig)
	// 	done <- true
	// }()

	// // The program will wait here until it gets the
	// // expected signal (as indicated by the goroutine
	// // above sending a value on `done`) and then exit.
	// fmt.Println("awaiting signal")
	// <-done
	// fmt.Println("exiting")
	// stop <- struct{}{}

}
