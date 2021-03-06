package processing

// DCSO FEVER
// Copyright (c) 2017, 2020, DCSO GmbH

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/DCSO/fever/types"
	"github.com/DCSO/fever/util"

	log "github.com/sirupsen/logrus"
)

// ForwardHandlerPerfStats contains performance stats written to InfluxDB
// for monitoring.
type ForwardHandlerPerfStats struct {
	ForwardedPerSec uint64 `influx:"forwarded_events_per_sec"`
}

// ForwardHandler is a handler that processes events by writing their JSON
// representation into a UNIX socket. This is limited by a list of allowed
// event types to be forwarded.
type ForwardHandler struct {
	Logger              *log.Entry
	DoRDNS              bool
	RDNSHandler         *RDNSHandler
	AddedFields         string
	ContextCollector    *ContextCollector
	StenosisIface       string
	StenosisConnector   *StenosisConnector
	ForwardEventChan    chan []byte
	FlowNotifyChan      chan types.Entry
	OutputSocket        string
	OutputConn          net.Conn
	Reconnecting        bool
	ReconnLock          sync.Mutex
	ReconnectNotifyChan chan bool
	StopReconnectChan   chan bool
	ReconnectTimes      int
	PerfStats           ForwardHandlerPerfStats
	StatsEncoder        *util.PerformanceStatsEncoder
	StopChan            chan bool
	StoppedChan         chan bool
	StopCounterChan     chan bool
	StoppedCounterChan  chan bool
	Running             bool
	Lock                sync.Mutex
}

func (fh *ForwardHandler) reconnectForward() {
	for range fh.ReconnectNotifyChan {
		var i int
		log.Info("Reconnecting to forwarding socket...")
		outputConn, myerror := net.Dial("unix", fh.OutputSocket)
		fh.ReconnLock.Lock()
		if !fh.Reconnecting {
			fh.Reconnecting = true
		} else {
			fh.ReconnLock.Unlock()
			continue
		}
		fh.ReconnLock.Unlock()
		for i = 0; (fh.ReconnectTimes == 0 || i < fh.ReconnectTimes) && myerror != nil; i++ {
			select {
			case <-fh.StopReconnectChan:
				return
			default:
				log.WithFields(log.Fields{
					"domain":     "forward",
					"retry":      i + 1,
					"maxretries": fh.ReconnectTimes,
				}).Warnf("error connecting to output socket, retrying: %s", myerror)
				time.Sleep(10 * time.Second)
				outputConn, myerror = net.Dial("unix", fh.OutputSocket)
			}
		}
		if myerror != nil {
			log.WithFields(log.Fields{
				"domain":  "forward",
				"retries": i,
			}).Fatalf("permanent error connecting to output socket: %s", myerror)
		} else {
			if i > 0 {
				log.WithFields(log.Fields{
					"domain":         "forward",
					"retry_attempts": i,
				}).Infof("connection to output socket successful")
			}
			fh.Lock.Lock()
			fh.OutputConn = outputConn
			fh.Lock.Unlock()
			fh.ReconnLock.Lock()
			fh.Reconnecting = false
			fh.ReconnLock.Unlock()
		}
	}
}

func (fh *ForwardHandler) runForward() {
	var err error
	for {
		select {
		case <-fh.StopChan:
			close(fh.StoppedChan)
			return
		default:
			for item := range fh.ForwardEventChan {
				select {
				case <-fh.StopChan:
					close(fh.StoppedChan)
					return
				default:
					fh.ReconnLock.Lock()
					if fh.Reconnecting {
						fh.ReconnLock.Unlock()
						continue
					}
					fh.ReconnLock.Unlock()
					fh.Lock.Lock()
					if fh.OutputConn != nil {
						_, err = fh.OutputConn.Write(item)
						if err != nil {
							fh.OutputConn.Close()
							fh.Lock.Unlock()
							log.Warn(err)
							fh.ReconnectNotifyChan <- true
							continue
						}
						_, err = fh.OutputConn.Write([]byte("\n"))
						if err != nil {
							fh.OutputConn.Close()
							fh.Lock.Unlock()
							log.Warn(err)
							continue
						}
					}
					fh.Lock.Unlock()
				}
			}
		}
	}
}

func (fh *ForwardHandler) runCounter() {
	sTime := time.Now()
	for {
		time.Sleep(500 * time.Millisecond)
		select {
		case <-fh.StopCounterChan:
			close(fh.StoppedCounterChan)
			return
		default:
			if fh.StatsEncoder == nil || time.Since(sTime) < fh.StatsEncoder.SubmitPeriod {
				continue
			}
			// Lock the current measurements for submission. Since this is a blocking
			// operation, we don't want this to depend on how long submitter.Submit()
			// takes but keep it independent of that. Hence we take the time to create
			// a local copy of the counter to be able to reset and release the live
			// one as quickly as possible.
			fh.Lock.Lock()
			// Make our own copy of the current counter
			myStats := ForwardHandlerPerfStats{
				ForwardedPerSec: fh.PerfStats.ForwardedPerSec,
			}
			myStats.ForwardedPerSec /= uint64(fh.StatsEncoder.SubmitPeriod.Seconds())
			// Reset live counter
			fh.PerfStats.ForwardedPerSec = 0
			// Release live counter to not block further events
			fh.Lock.Unlock()

			fh.StatsEncoder.Submit(myStats)
			sTime = time.Now()
		}
	}
}

// MakeForwardHandler creates a new forwarding handler
func MakeForwardHandler(reconnectTimes int, outputSocket string) *ForwardHandler {
	fh := &ForwardHandler{
		Logger: log.WithFields(log.Fields{
			"domain": "forward",
		}),
		OutputSocket:        outputSocket,
		ReconnectTimes:      reconnectTimes,
		ReconnectNotifyChan: make(chan bool),
		StopReconnectChan:   make(chan bool),
	}
	return fh
}

// Consume processes an Entry and prepares it to be sent off to the
// forwarding sink
func (fh *ForwardHandler) Consume(e *types.Entry) error {
	doForwardThis := util.ForwardAllEvents || util.AllowType(e.EventType)
	if doForwardThis {
		// mark flow as relevant when alert is seen
		if GlobalContextCollector != nil && e.EventType == types.EventTypeAlert {
			GlobalContextCollector.Mark(string(e.FlowID))
		}
		// we also perform active rDNS enrichment if requested
		if fh.DoRDNS && fh.RDNSHandler != nil {
			err := fh.RDNSHandler.Consume(e)
			if err != nil {
				return err
			}
		}
		// Replace the final brace `}` in the JSON with the prepared string to
		// add the 'added fields' defined in the config. I the length of this
		// string is 1 then there are no added fields, only a final brace '}'.
		// In this case we don't even need to modify the JSON string at all.
		if len(fh.AddedFields) > 1 {
			j := e.JSONLine
			l := len(j)
			j = j[:l-1]
			j += fh.AddedFields
			e.JSONLine = j
		}
		// if we use Stenosis, the Stenosis connector will take ownership of
		// alerts
		if fh.StenosisConnector != nil &&
			e.EventType == types.EventTypeAlert &&
			(fh.StenosisIface == "*" || e.Iface == fh.StenosisIface) {
			fh.StenosisConnector.Accept(e)
		} else {
			fh.ForwardEventChan <- []byte(e.JSONLine)
			fh.Lock.Lock()
			fh.PerfStats.ForwardedPerSec++
			fh.Lock.Unlock()
		}
	}
	return nil
}

// GetName returns the name of the handler
func (fh *ForwardHandler) GetName() string {
	return "Forwarding handler"
}

// GetEventTypes returns a slice of event type strings that this handler
// should be applied to
func (fh *ForwardHandler) GetEventTypes() []string {
	if util.ForwardAllEvents {
		return []string{"*"}
	}
	return util.GetAllowedTypes()
}

// EnableRDNS switches on reverse DNS enrichment for source and destination
// IPs in outgoing EVE events.
func (fh *ForwardHandler) EnableRDNS(expiryPeriod time.Duration) {
	fh.DoRDNS = true
	fh.RDNSHandler = MakeRDNSHandler(util.NewHostNamerRDNS(expiryPeriod, 2*expiryPeriod))
}

// AddFields enables the addition of a custom set of top-level fields to the
// forwarded JSON.
func (fh *ForwardHandler) AddFields(fields map[string]string) error {
	j := ""
	// We preprocess the JSON to be able to only use fast string operations
	// later. This code progressively builds a JSON snippet by adding JSON
	// key-value pairs for each added field, e.g. `, "foo":"bar"`.
	for k, v := range fields {
		// Escape the fields to make sure we do not mess up the JSON when
		// encountering weird symbols in field names or values.
		kval, err := util.EscapeJSON(k)
		if err != nil {
			fh.Logger.Warningf("cannot escape value: %s", v)
			return err
		}
		vval, err := util.EscapeJSON(v)
		if err != nil {
			fh.Logger.Warningf("cannot escape value: %s", v)
			return err
		}
		j += fmt.Sprintf(",%s:%s", kval, vval)
	}
	// We finish the list of key-value pairs with a final brace:
	// `, "foo":"bar"}`. This string can now just replace the final brace in a
	// given JSON string. If there were no added fields, we just leave the
	// output at the final brace.
	j += "}"
	fh.AddedFields = j
	return nil
}

// EnableStenosis ...
func (fh *ForwardHandler) EnableStenosis(endpoint string, timeout, timeBracket time.Duration,
	notifyChan chan types.Entry, cacheExpiry time.Duration, tlsConfig *tls.Config, iface string) (err error) {
	fh.StenosisConnector, err = MakeStenosisConnector(endpoint, timeout, timeBracket,
		notifyChan, fh.ForwardEventChan, cacheExpiry, tlsConfig)
	fh.StenosisIface = iface
	return
}

// Run starts forwarding of JSON representations of all consumed events
func (fh *ForwardHandler) Run() {
	if !fh.Running {
		fh.StopChan = make(chan bool)
		fh.ForwardEventChan = make(chan []byte, 10000)
		fh.StopCounterChan = make(chan bool)
		fh.StoppedCounterChan = make(chan bool)
		go fh.reconnectForward()
		fh.ReconnectNotifyChan <- true
		go fh.runForward()
		go fh.runCounter()
		fh.Running = true
	}
}

// Stop stops forwarding of JSON representations of all consumed events
func (fh *ForwardHandler) Stop(stoppedChan chan bool) {
	if fh.Running {
		close(fh.StopCounterChan)
		<-fh.StoppedCounterChan
		fh.StoppedChan = stoppedChan
		fh.Lock.Lock()
		fh.OutputConn.Close()
		fh.Lock.Unlock()
		close(fh.StopReconnectChan)
		close(fh.StopChan)
		close(fh.ForwardEventChan)
		fh.Running = false
	}
}

// SubmitStats registers a PerformanceStatsEncoder for runtime stats submission.
func (fh *ForwardHandler) SubmitStats(sc *util.PerformanceStatsEncoder) {
	fh.StatsEncoder = sc
}
