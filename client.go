package http

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/common/transport"
	"github.com/elastic/beats/v7/libbeat/common/transport/tlscommon"
	"github.com/elastic/beats/v7/libbeat/logp"
	"github.com/elastic/beats/v7/libbeat/outputs"
	"github.com/elastic/beats/v7/libbeat/outputs/outil"
	"github.com/elastic/beats/v7/libbeat/publisher"
)

// Client struct
type Client struct {
	Connection
	tlsConfig *tlscommon.TLSConfig
	params    map[string]string
	// additional configs
	compressionLevel int
	proxyURL         *url.URL
	batchPublish     bool
	observer         outputs.Observer
	headers          map[string]string
	format           string
	log              *logp.Logger
}

// ClientSettings struct
type ClientSettings struct {
	URL                string
	Proxy              *url.URL
	TLS                *tlscommon.TLSConfig
	Username, Password string
	Parameters         map[string]string
	Index              outil.Selector
	Pipeline           *outil.Selector
	Timeout            time.Duration
	CompressionLevel   int
	Observer           outputs.Observer
	BatchPublish       bool
	Headers            map[string]string
	ContentType        string
	Format             string
}

// Connection struct
type Connection struct {
	URL         string
	Username    string
	Password    string
	http        *http.Client
	connected   bool
	encoder     bodyEncoder
	ContentType string
}

type eventRaw map[string]json.RawMessage

type event struct {
	Timestamp time.Time     `json:"@timestamp"`
	Fields    common.MapStr `json:"-"`
}

// var allSeverities map[string]bool
var (
	allSeverities = map[string]bool{
		"1": true,
		"2": true,
		"3": true,
		"4": true,
		"5": true,
		"6": true,
	}
)

// NewClient instantiate a client.
func NewClient(s ClientSettings) (*Client, error) {
	proxy := http.ProxyFromEnvironment
	if s.Proxy != nil {
		proxy = http.ProxyURL(s.Proxy)
	}
	// logger.Info("HTTP URL: %s", s.URL)
	var dialer, tlsDialer transport.Dialer
	var err error

	dialer = transport.NetDialer(s.Timeout)
	tlsDialer, err = transport.TLSDialer(dialer, s.TLS, s.Timeout)
	if err != nil {
		return nil, err
	}

	if st := s.Observer; st != nil {
		dialer = transport.StatsDialer(dialer, st)
		tlsDialer = transport.StatsDialer(tlsDialer, st)
	}
	params := s.Parameters
	var encoder bodyEncoder
	compression := s.CompressionLevel
	if compression == 0 {
		switch s.Format {
		case "json":
			encoder = newJSONEncoder(nil)
		case "json_lines":
			encoder = newJSONLinesEncoder(nil)
		}
	} else {
		switch s.Format {
		case "json":
			encoder, err = newGzipEncoder(compression, nil)
		case "json_lines":
			encoder, err = newGzipLinesEncoder(compression, nil)
		}
		if err != nil {
			return nil, err
		}
	}
	client := &Client{
		Connection: Connection{
			URL:         s.URL,
			Username:    s.Username,
			Password:    s.Password,
			ContentType: s.ContentType,
			http: &http.Client{
				Transport: &http.Transport{
					Dial:    dialer.Dial,
					DialTLS: tlsDialer.Dial,
					Proxy:   proxy,
				},
				Timeout: s.Timeout,
			},
			encoder: encoder,
		},
		params:           params,
		compressionLevel: compression,
		proxyURL:         s.Proxy,
		batchPublish:     s.BatchPublish,
		headers:          s.Headers,
		format:           s.Format,
		log:              logp.NewLogger("coralogix"),
	}

	return client, nil
}

// Clone clones a client.
func (client *Client) Clone() *Client {
	// when cloning the connection callback and params are not copied. A
	// client's close is for example generated for topology-map support. With params
	// most likely containing the ingest node pipeline and default callback trying to
	// create install a template, we don't want these to be included in the clone.
	c, _ := NewClient(
		ClientSettings{
			URL:              client.URL,
			Proxy:            client.proxyURL,
			TLS:              client.tlsConfig,
			Username:         client.Username,
			Password:         client.Password,
			Parameters:       client.params,
			Timeout:          client.http.Timeout,
			CompressionLevel: client.compressionLevel,
			BatchPublish:     client.batchPublish,
			Headers:          client.headers,
			ContentType:      client.ContentType,
			Format:           client.format,
		},
	)
	return c
}

// Connect establishes a connection to the clients sink.
func (conn *Connection) Connect() error {
	conn.connected = true
	return nil
}

// Close closes a connection.
func (conn *Connection) Close() error {
	conn.connected = false
	return nil
}

func (client *Client) String() string {
	return client.URL
}

// Publish sends events to the clients sink.
func (client *Client) Publish(_ context.Context, batch publisher.Batch) error {
	events := batch.Events()
	rest, err := client.publishEvents(events)
	if len(rest) == 0 {
		batch.ACK()
	} else {
		batch.RetryEvents(rest)
	}
	return err
}

// PublishEvents posts all events to the http endpoint. On error a slice with all
// events not published will be returned.
func (client *Client) publishEvents(data []publisher.Event) ([]publisher.Event, error) {
	begin := time.Now()
	if len(data) == 0 {
		return nil, nil
	}
	if !client.connected {
		return data, ErrNotConnected
	}
	var failedEvents []publisher.Event
	sendErr := error(nil)
	if client.batchPublish {
		// Publish events in bulk
		client.log.Debugf("Publishing events in batch.")
		sendErr = client.BatchPublishEvent(data)
		if sendErr != nil {
			return data, sendErr
		}
	} else {
		client.log.Debugf("Publishing events one by one.")
		for index, event := range data {
			sendErr = client.PublishEvent(event)
			if sendErr != nil {
				// return the rest of the data with the error
				failedEvents = data[index:]
				break
			}
		}
	}
	client.log.Debugf("PublishEvents: %d metrics have been published over HTTP in %v.", len(data), time.Now().Sub(begin))
	if len(failedEvents) > 0 {
		return failedEvents, sendErr
	}
	return nil, nil
}

// BatchPublishEvent publish a single event to output.
func (client *Client) BatchPublishEvent(data []publisher.Event) error {
	if !client.connected {
		return ErrNotConnected
	}
	client.log.Debugf("BATCH:: Start batch of %d events", len(data))
	var events = make([]eventRaw, len(data))
	for i, event := range data {
		events[i] = client.makeEvent(&event.Content)
	}
	status, _, err := client.request("POST", client.params, events, client.headers)
	if err != nil {
		client.log.Warn("Fail to insert a single event: %s", err)
		if err == ErrJSONEncodeFailed {
			// don't retry unencodable values
			return nil
		}
	}
	client.log.Debugf("BATCH:: status code %s", status)
	switch {
	case status == 500 || status == 400: //server error or bad input, don't retry
		return nil
	case status >= 300:
		// retry
		return err
	}
	client.log.Debugf("BATCH:: END OF BATCH")
	return nil
}

// PublishEvent publish a single event to output.
func (client *Client) PublishEvent(data publisher.Event) error {
	if !client.connected {
		return ErrNotConnected
	}
	event := data
	client.log.Debugf("CORALOGIXX LOGGER")
	client.log.Debugf("Publish event: %s", event)
	status, _, err := client.request("POST", client.params, client.makeEvent(&event.Content), client.headers)
	if err != nil {
		client.log.Warn("Fail to insert a single event: %s", err)
		if err == ErrJSONEncodeFailed {
			// don't retry unencodable values
			return nil
		}
	}
	switch {
	case status == 500 || status == 400: //server error or bad input, don't retry
		return nil
	case status >= 300:
		// retry
		return err
	}
	if !client.connected {
		return ErrNotConnected
	}
	return nil
}

func (conn *Connection) request(method string, params map[string]string, body interface{}, headers map[string]string) (int, []byte, error) {
	urlStr := addToURL(conn.URL, params)
	// logger.Debugf("%s %s %v", method, urlStr, body)

	if body == nil {
		return conn.execRequest(method, urlStr, nil, headers)
	}

	if err := conn.encoder.Marshal(body); err != nil {
		// logger.Warn("Failed to json encode body (%v): %#v", err, body)
		return 0, nil, ErrJSONEncodeFailed
	}
	return conn.execRequest(method, urlStr, conn.encoder.Reader(), headers)
}

func (conn *Connection) execRequest(method, url string, body io.Reader, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		// logger.Warn("Failed to create request: %v", err)
		return 0, nil, err
	}
	if body != nil {
		conn.encoder.AddHeader(&req.Header, conn.ContentType)
	}
	return conn.execHTTPRequest(req, headers)
}

func (conn *Connection) execHTTPRequest(req *http.Request, headers map[string]string) (int, []byte, error) {
	req.Header.Add("Accept", "application/json")
	for key, value := range headers {
		req.Header.Add(key, value)
	}
	if conn.Username != "" || conn.Password != "" {
		req.SetBasicAuth(conn.Username, conn.Password)
	}
	resp, err := conn.http.Do(req)
	if err != nil {
		conn.connected = false
		return 0, nil, err
	}
	defer closing(resp.Body)

	status := resp.StatusCode
	if status >= 300 {
		conn.connected = false
		return status, nil, fmt.Errorf("%v", resp.Status)
	}
	obj, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		conn.connected = false
		return status, nil, err
	}
	return status, obj, nil
}

func closing(c io.Closer) {
	err := c.Close()
	if err != nil {
		// logger.Warn("Close failed with: %v", err)
	}
}

//this should ideally be in enc.go
func (client *Client) makeEvent(v *beat.Event) map[string]json.RawMessage {
	// Inline not supported,
	// HT: https://stackoverflow.com/questions/49901287/embed-mapstringstring-in-go-json-marshaling-without-extra-json-property-inlin
	type event0 event // prevent recursion

	e := event{Timestamp: v.Timestamp.UTC(), Fields: v.Fields}
	b, err := json.Marshal(event0(e))
	if err != nil {
		client.log.Warn("Error encoding event to JSON: %v", err)
	}

	var eventMap map[string]json.RawMessage
	err = json.Unmarshal(b, &eventMap)
	if err != nil {
		client.log.Warn("Error decoding JSON to map: %v", err)
	}

	// coralogix parameters
	client.log.Debug("ADDING CORALOGIX PARAMETERS::")

	cxParamsKeys := []string{"applicationName", "subsystemName", "privateKey"}

	for _, k := range cxParamsKeys {
		paramVal, err := e.Fields.GetValue(k)
		if err != nil {
			client.log.Warn("Error Coralogix parameter %v is not exists: %v ", k, err)
		}
		b, err = json.Marshal(paramVal.(string))
		if err != nil {
			client.log.Warn("Error encoding map to JSON: %v", err)
		}
		eventMap[k] = b
		e.Fields.Delete(k)

	}
	/////////////////////////////////// END OF HEADERS

	/////////////////////////////////// START OF SPECIFIC EVENTS

	var messageMap map[string]json.RawMessage

	isMessage, _ := e.Fields.HasKey("message")
	isSeverityInMessage := false
	isTimestampInMessage := false
	var severityVal interface{} = "3"

	if isMessage {
		messageVal, _ := e.Fields.GetValue("message")
		messageStr, _ := messageVal.(string)
		err = json.Unmarshal([]byte(messageStr), &messageMap)

		SevInMessage, _ := messageMap["severity"]
		// severityVal, _ = SevInMessage.MarshalJSON()
		isSeverityInMessage = string(SevInMessage) != "null"
		if isSeverityInMessage {
			severityVal = string(SevInMessage)
		}
		// isSeverityInMessage = allSeverities[string(SevInMessage)]
		client.log.Debug("sevInMessage ::::::::" + string(SevInMessage))

		TSInMessage, _ := messageMap["timestamp"]
		// TSInMessageVal, _ := TSInMessage.MarshalJSON()
		isTimestampInMessage = string(TSInMessage) != "null"

	}
	client.log.Debug("isSeverityInMessage ::::::::" + strconv.FormatBool(isSeverityInMessage))
	client.log.Debug("isTimestampInMessage ::::::::" + strconv.FormatBool(isTimestampInMessage))

	// var severityVal interface{} = "3"
	isSeverityInRoot, err := e.Fields.HasKey("severity")
	if isSeverityInMessage || isSeverityInRoot || isTimestampInMessage {
		if isSeverityInRoot {
			severityVal, _ = e.Fields.GetValue("severity")
			e.Fields.Delete("severity")
		}
		//  else {
		// 	severityVal = severityVal
		// }

	} else {
		severityVal = "3"
	}

	// if "timestamp" does not exist put current epoch time,
	// if exist save it and delete from e.Fields
	var timestampVal interface{} = "11111111111"
	_, err = e.Fields.HasKey("timestamp")
	if err != nil {
		timestampVal, _ = e.Fields.GetValue("timestamp")
		e.Fields.Delete("timestamp")
	} else {
		epochTimeInt := int64(time.Now().UnixNano() / int64(time.Millisecond))
		timestampVal = strconv.FormatInt(int64(epochTimeInt), 10)
	}

	//////////////////////////////////////TEST//////////////////////////////////////////
	// var textMap map[string]json.RawMessage
	// textVal, _ := e.Fields.GetValue("message")
	// textStr, _ := textVal.(string)
	// client.log.Info("MESSAGE.TEXT::::::::: " + string(textStr))
	// err = json.Unmarshal([]byte(textStr), &textMap)
	// sevVal, _ := textMap["severity"]
	// sevvall, _ := sevVal.MarshalJSON()
	// client.log.Info("MESSAGE.SEVERITY::::::::: " + string(sevvall))
	////////////////////////////////////////TEST/////////////////////////////////////////

	// add log entries fields
	cxParamsInterface := []map[string]interface{}{{"timestamp": timestampVal, "severity": severityVal, "text": common.MapStr.String(e.Fields)}}

	b, err = json.Marshal(cxParamsInterface)

	if err != nil {
		client.log.Warn("Error encoding map to JSON: %v", err)
	}
	eventMap["logEntries"] = b

	////////////////////////////////////////////////////////////////////////////////////

	return eventMap
}

func (client *Client) makeEventHeader(v *beat.Event) map[string]json.RawMessage {
	type event0 event // prevent recursion

	e := event{Timestamp: v.Timestamp.UTC(), Fields: v.Fields}
	b, err := json.Marshal(event0(e))
	if err != nil {
		client.log.Warn("Error encoding event to JSON: %v", err)
	}

	var eventMap map[string]json.RawMessage
	err = json.Unmarshal(b, &eventMap)
	if err != nil {
		client.log.Warn("Error decoding JSON to map: %v", err)
	}

	// coralogix parameters
	client.log.Debug("ADDING CORALOGIX PARAMETERS::")

	cxParamsKeys := []string{"applicationName", "subsystemName", "privateKey"}

	for _, k := range cxParamsKeys {
		paramVal, err := e.Fields.GetValue(k)
		if err != nil {
			client.log.Warn("Error Coralogix parameter %v is not exists: %v ", k, err)
		}
		b, err = json.Marshal(paramVal.(string))
		if err != nil {
			client.log.Warn("Error encoding map to JSON: %v", err)
		}
		eventMap[k] = b
		e.Fields.Delete(k)

	}
	return eventMap
}
