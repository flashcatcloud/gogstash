package outputelastic

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/olivere/elastic/v7"
	"github.com/sirupsen/logrus"
	"github.com/tsaikd/KDGoLib/errutil"
	"github.com/tsaikd/gogstash/config"
	"github.com/tsaikd/gogstash/config/goglog"
	"github.com/tsaikd/gogstash/config/logevent"
)

// ModuleName is the name used in config file
const ModuleName = "elastic"

// OutputConfig holds the configuration json fields and internal objects
type OutputConfig struct {
	config.OutputConfig
	URL             []string `json:"url"` // elastic API entrypoints
	resolvedURLs    []string // URLs after resolving environment vars
	Index           string   `json:"index"`             // index name to log
	DocumentType    string   `json:"document_type"`     // type name to log
	DocumentID      string   `json:"document_id"`       // id to log, used if you want to control id format
	RetryOnConflict int      `json:"retry_on_conflict"` // the number of times Elasticsearch should internally retry an update/upserted document
	Username        string   `json:"username"`          // basic auth username to Elasticsearch
	Password        string   `json:"password"`          // basic auth password to Elasticsearch
	SimpleClient    bool     `json:"simple_client"`     // if set uses simpleclient instead of newclient, disables some functionality

	TimestampIndexField  string `json:"timestamp_index_field"`  // which field use to index
	TimestampIndexFormat string `json:"timestamp_index_format"` // the field's time format

	Sniff bool `json:"sniff"` // find all nodes of your cluster, https://github.com/olivere/elastic/wiki/Sniffing

	// BulkActions specifies when to flush based on the number of actions
	// currently added. Defaults to 1000 and can be set to -1 to be disabled.
	BulkActions int `json:"bulk_actions,omitempty"`

	// BulkSize specifies when to flush based on the size (in bytes) of the actions
	// currently added. Defaults to 5 MB and can be set to -1 to be disabled.
	BulkSize int `json:"bulk_size,omitempty"`

	// BulkFlushInterval specifies when to flush at the end of the given interval.
	// Defaults to 30 seconds. If you want the bulk processor to
	// operate completely asynchronously, set both BulkActions and BulkSize to
	// -1 and set the FlushInterval to a meaningful interval.
	BulkFlushInterval time.Duration `json:"bulk_flush_interval"`

	// ExponentialBackoffInitialTimeout used to set the first/minimal interval in elastic.ExponentialBackoff
	// Defaults to 10s
	ExponentialBackoffInitialTimeout string `json:"exponential_backoff_initial_timeout,omitempty"`
	exponentialBackoffInitialTimeout time.Duration

	// ExponentialBackoffMaxTimeout used to set the maximum wait interval in elastic.ExponentialBackoff
	// Defaults to 5m
	ExponentialBackoffMaxTimeout string `json:"exponential_backoff_max_timeout,omitempty"`
	exponentialBackoffMaxTimeout time.Duration

	// SSLCertValidation Option to validate the server's certificate. Disabling this severely compromises security.
	// For more information on disabling certificate verification please read https://www.cs.utexas.edu/~shmat/shmat_ccs12.pdf
	SSLCertValidation bool `json:"ssl_certificate_validation,omitempty"`

	client    *elastic.Client        // elastic client instance
	processor *elastic.BulkProcessor // elastic bulk processor
}

// DefaultOutputConfig returns an OutputConfig struct with default values
func DefaultOutputConfig() OutputConfig {
	return OutputConfig{
		OutputConfig: config.OutputConfig{
			CommonConfig: config.CommonConfig{
				Type: ModuleName,
			},
		},
		RetryOnConflict:                  1,
		BulkActions:                      1000,    // 1000 actions
		BulkSize:                         5 << 20, // 5 MB
		BulkFlushInterval:                30 * time.Second,
		ExponentialBackoffInitialTimeout: "10s",
		ExponentialBackoffMaxTimeout:     "5m",
		SSLCertValidation:                true,
	}
}

// errors
var (
	ErrorCreateClientFailed1 = errutil.NewFactory("create elastic client failed: %q")
)

type errorLogger struct {
	logger logrus.FieldLogger
}

// Printf log format string to error level
func (l *errorLogger) Printf(format string, args ...interface{}) {
	l.logger.Errorf(format, args...)
}

type jsonDecoder struct{}

// Decode decodes with jsoniter.Unmarshal
func (u *jsonDecoder) Decode(data []byte, v interface{}) error {
	return jsoniter.Unmarshal(data, v)
}

// InitHandler initialize the output plugin
func InitHandler(
	ctx context.Context,
	raw config.ConfigRaw,
	control config.Control,
) (config.TypeOutputConfig, error) {
	conf := DefaultOutputConfig()
	err := config.ReflectConfig(raw, &conf)
	if err != nil {
		return nil, err
	}

	// map Printf to error level
	logger := &errorLogger{logger: goglog.Logger}

	// replace env var names with values on URL config
	for _, url := range conf.URL {
		newURL := logevent.FormatWithEnv(url)
		conf.resolvedURLs = append(conf.resolvedURLs, newURL)
	}

	options := []elastic.ClientOptionFunc{
		elastic.SetURL(conf.resolvedURLs...),
		elastic.SetSniff(conf.Sniff),
		elastic.SetErrorLog(logger),
		elastic.SetBasicAuth(conf.Username, conf.Password),
		elastic.SetDecoder(&jsonDecoder{}),
	}

	// set httpclient explicitly if we need to avoid https cert checks
	if !conf.SSLCertValidation {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
		client := &http.Client{Transport: tr}
		options = append(options, elastic.SetHttpClient(client))
	}

	if conf.SimpleClient {
		conf.client, err = elastic.NewSimpleClient(options...)
	} else {
		conf.client, err = elastic.NewClient(options...)
	}
	if err != nil {
		return nil, ErrorCreateClientFailed1.New(err, conf.URL)
	}

	conf.exponentialBackoffInitialTimeout, err = time.ParseDuration(conf.ExponentialBackoffInitialTimeout)
	if err != nil {
		return nil, err
	}

	conf.exponentialBackoffMaxTimeout, err = time.ParseDuration(conf.ExponentialBackoffMaxTimeout)
	if err != nil {
		return nil, err
	}

	conf.processor, err = conf.client.BulkProcessor().
		Name("gogstash-output-elastic").
		BulkActions(conf.BulkActions).
		BulkSize(conf.BulkSize).
		FlushInterval(conf.BulkFlushInterval).
		Backoff(elastic.NewExponentialBackoff(conf.exponentialBackoffInitialTimeout, conf.exponentialBackoffMaxTimeout)).
		After(conf.BulkAfter).
		Do(ctx)
	if err != nil {
		return nil, err
	}

	return &conf, nil
}

// BulkAfter execute after a commit to Elasticsearch
func (t *OutputConfig) BulkAfter(executionID int64, requests []elastic.BulkableRequest, response *elastic.BulkResponse, err error) {
	if err == nil && response.Errors {
		// find failed requests, and log it
		for i, item := range response.Items {
			for _, v := range item {
				if v.Error != nil {
					goglog.Logger.Errorf("%s: bulk processor request %s failed: %s", ModuleName, requests[i].String(), v.Error.Reason)
				}
			}
		}
	}
}

// Output event
func (t *OutputConfig) Output(ctx context.Context, event logevent.LogEvent) (err error) {
	index := t.Index
	// time_format eg. "2006-01-02T15:04:05.999Z"
	if t.TimestampIndexField != "" && t.TimestampIndexFormat != "" {
		if v, ok := event.GetValue(t.TimestampIndexField); ok {
			switch vv := v.(type) {
			case string:
				if eventTime, err := time.Parse(t.TimestampIndexFormat, vv); err == nil {
					index = logevent.FormatWithEventTime(t.Index, eventTime)
					goglog.Logger.Debugf("prepare to create elastic index %s with time field %s, format %s and eventtime %s",
						index, t.TimestampIndexField, t.TimestampIndexFormat, eventTime)
				} else {
					goglog.Logger.Errorf("timestamp index field %s has an invalid parse format %s, "+
						"field value %s with parse error %s",
						t.TimestampIndexField, t.TimestampIndexFormat, v, err)
				}
			case time.Time:
				index = vv.Format(t.TimestampIndexFormat)
			default:
				goglog.Logger.Warnf("type of field %s is invalid", t.TimestampIndexField, event.Extra)
			}
		} else {
			goglog.Logger.Warnf("event get %s value failed %+v", t.TimestampIndexField, event.Extra)
		}
	}
	index = event.Format(index)
	// elastic index name should be lowercase
	index = strings.ToLower(index)
	goglog.Logger.Debugf("create elastic index %s", index)
	id := event.Format(t.DocumentID)

	indexRequest := elastic.NewBulkIndexRequest().
		Index(index).
		RetryOnConflict(t.RetryOnConflict).
		Id(id).
		Doc(event)
	t.processor.Add(indexRequest)

	return
}
