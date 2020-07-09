/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2020 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package cmd

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/crewjam/rfc5424"
	"github.com/sirupsen/logrus"
)

// TODO move this to it's own package
// reconnect?
// filtering? limiting output? maybe? probably leave it for syslog-ng/rsyslog and co ?
// benchmark it
// buffer messages before sending them

// loosely based on https://godoc.org/github.com/sirupsen/logrus/hooks/syslog
type syslogHook struct {
	Writer           net.Conn
	protocol         string
	addr             string
	additionalParams [][2]string
	ch               chan *logrus.Entry
	limit            int
	levels           []logrus.Level
	pushPeriod       time.Duration
}

func syslogFromConfigLine(line string) (*syslogHook, error) {
	h := &syslogHook{
		protocol:   "tcp",
		addr:       "localhost:514",
		limit:      100,
		levels:     logrus.AllLevels,
		pushPeriod: time.Second * 1, // TODO configurable,
	}
	if line == "syslog" {
		return h, nil
	}

	parts := strings.SplitN(line, "=", 2)
	if parts[0] != "syslog" {
		return nil, fmt.Errorf("syslog  configuration should be in the form `syslog=host:port` but is `%s`", logOutput)
	}
	args := strings.Split(parts[1], ",")
	h.addr = args[0]
	// TODO use something better ... maybe
	// https://godoc.org/github.com/kubernetes/helm/pkg/strvals
	// atleast until https://github.com/loadimpact/k6/issues/926?
	if len(args) == 1 {
		return h, nil
	}

	for _, arg := range args[1:] {
		paramParts := strings.SplitN(arg, "=", 2)

		if len(paramParts) != 2 {
			return nil, fmt.Errorf("syslog arguments should be in the form `address,key1=value1,key2=value2`, got %s", arg)
		}

		key := paramParts[0]
		value := paramParts[1]
		switch key {
		case "additionalParams":
			values := strings.Split(value, ";") // ; because , is already used

			h.additionalParams = make([][2]string, len(values))
			for i, value := range values {
				additionalParamParts := strings.SplitN(value, "=", 2)
				if len(additionalParamParts) != 2 {
					return nil, fmt.Errorf("additionalparam should be in the form key1=value1;key2=value2, got %s", value)
				}
				h.additionalParams[i] = [2]string{additionalParamParts[0], additionalParamParts[1]}
			}
		case "limit":
			var err error
			h.limit, err = strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("couldn't parse the syslog limit as a number %w", err)
			}
		case "level":
			// TODO figure out if `tracing`,`fatal` and `panic` should be included
			h.levels = []logrus.Level{}
			switch value {
			case "debug":
				h.levels = append(h.levels, logrus.DebugLevel)
				fallthrough
			case "info":
				h.levels = append(h.levels, logrus.InfoLevel)
				fallthrough
			case "warning":
				h.levels = append(h.levels, logrus.WarnLevel)
				fallthrough
			case "error":
				h.levels = append(h.levels, logrus.ErrorLevel)
			default:
				return nil, fmt.Errorf("unknown log level %s", value)
			}
		default:
			return nil, fmt.Errorf("unknown syslog config key %s", key)
		}
	}

	return h, nil
}

func (h *syslogHook) start() error {
	w, err := net.Dial(h.protocol, h.addr)
	h.Writer = w
	h.ch = make(chan *logrus.Entry, 1000)
	go h.loop()

	return err
}

// fill one of two equally sized slices with entries and then push it while filling the other one
// TODO clean old entries after push?
// TODO this will be much faster if we can reuse rfc5424.Messages and they can use less intermediary
// buffers
func (h *syslogHook) loop() {
	var (
		entrys             = make([]*logrus.Entry, h.limit)
		entriesBeingPushed = make([]*logrus.Entry, h.limit)
		dropped            int
		count              int
		ticker             = time.NewTicker(h.pushPeriod)
		pushCh             = make(chan chan struct{})
	)

	defer close(pushCh)
	go func() {
		for ch := range pushCh {
			entriesBeingPushed, entrys = entrys, entriesBeingPushed
			oldCount, oldDropped := count, dropped
			count, dropped = 0, 0
			close(ch)
			_ = h.push(entriesBeingPushed[:oldCount], oldDropped) // TODO print it on terminal ?!?
		}
	}()

	for {
		select {
		case entry, ok := <-h.ch:
			if !ok {
				return
			}
			if count == h.limit {
				dropped++
				continue
			}
			entrys[count] = entry
			count++
		case <-ticker.C:
			ch := make(chan struct{})
			pushCh <- ch
			<-ch
		}
	}
}

var b bytes.Buffer //nolint:nochecknoglobals // TODO maybe use sync.Pool?

func (h *syslogHook) push(entrys []*logrus.Entry, dropped int) error {
	b.Reset()
	for _, entry := range entrys {
		if _, err := msgFromEntry(entry, h.additionalParams).WriteTo(&b); err != nil {
			return err
		}
	}
	if dropped != 0 {
		_, err := msgFromEntry(
			&logrus.Entry{
				Data: logrus.Fields{
					"droppedCount": dropped,
				},
				Level: logrus.WarnLevel,
				Message: fmt.Sprintf("k6 dropped some packages because they were above the limit of %d/%s",
					h.limit, h.pushPeriod),
			},
			h.additionalParams,
		).WriteTo(&b)
		if err != nil {
			return err
		}
	}
	_, err := b.WriteTo(h.Writer)
	return err
}

func (h *syslogHook) Fire(entry *logrus.Entry) error {
	h.ch <- entry
	return nil
}

func msgFromEntry(entry *logrus.Entry, additionalParams [][2]string) rfc5424.Message {
	// TODO figure out if entrys share their entry.Data and use that to not recreate the same
	// sdParams
	sdParams := make([]rfc5424.SDParam, 1, 1+len(entry.Data)+len(additionalParams))
	sdParams[0] = rfc5424.SDParam{Name: "level", Value: entry.Level.String()}
	for name, value := range entry.Data {
		// TODO maybe do it only for some?
		// TODO have custom logic for different things ?
		sdParams = append(sdParams, rfc5424.SDParam{Name: name, Value: fmt.Sprint(value)})
	}

	for _, param := range additionalParams {
		sdParams = append(sdParams, rfc5424.SDParam{Name: param[0], Value: param[1]})
	}

	return rfc5424.Message{
		Priority:  rfc5424.Daemon | rfc5424.Info, // TODO figure this out
		Timestamp: entry.Time,
		Message:   []byte(entry.Message),
		StructuredData: []rfc5424.StructuredData{
			{
				ID:         "k6",
				Parameters: sdParams,
			},
		},
	}
}

func (h *syslogHook) Levels() []logrus.Level {
	return h.levels
}
