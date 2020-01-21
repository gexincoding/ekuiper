package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/emqx/kuiper/common"
	"github.com/emqx/kuiper/xsql/processors"
	"github.com/emqx/kuiper/xstream"
	"github.com/emqx/kuiper/xstream/api"
	"github.com/emqx/kuiper/xstream/sinks"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net"
	"net/http"
	"net/rpc"
	"path"
	"strings"
	"time"
)

var (
	dataDir         string
	log             = common.Log
	registry        RuleRegistry
	ruleProcessor   *processors.RuleProcessor
	streamProcessor *processors.StreamProcessor
)

const QUERY_RULE_ID = "internal-xstream_query_rule"

type RuleState struct {
	Name      string
	Topology  *xstream.TopologyNew
	Triggered bool
}
type RuleRegistry map[string]*RuleState

type Server int

func (t *Server) CreateQuery(sql string, reply *string) error {
	if _, ok := registry[QUERY_RULE_ID]; ok {
		stopQuery()
	}
	tp, err := processors.NewRuleProcessor(path.Dir(dataDir)).ExecQuery(QUERY_RULE_ID, sql)
	if err != nil {
		return err
	} else {
		rs := &RuleState{Name: QUERY_RULE_ID, Topology: tp, Triggered: true}
		registry[QUERY_RULE_ID] = rs
		msg := fmt.Sprintf("Query was submit successfully.")
		log.Println(msg)
		*reply = fmt.Sprintf(msg)
	}
	return nil
}

func stopQuery() {
	if rs, ok := registry[QUERY_RULE_ID]; ok {
		log.Printf("stop the query.")
		(*rs.Topology).Cancel()
		delete(registry, QUERY_RULE_ID)
	}
}

/**
 * qid is not currently used.
 */
func (t *Server) GetQueryResult(qid string, reply *string) error {
	if rs, ok := registry[QUERY_RULE_ID]; ok {
		c := (*rs.Topology).GetContext()
		if c != nil && c.Err() != nil {
			return c.Err()
		}
	}

	sinks.QR.LastFetch = time.Now()
	sinks.QR.Mux.Lock()
	if len(sinks.QR.Results) > 0 {
		*reply = strings.Join(sinks.QR.Results, "")
		sinks.QR.Results = make([]string, 10)
	} else {
		*reply = ""
	}
	sinks.QR.Mux.Unlock()
	return nil
}

func (t *Server) Stream(stream string, reply *string) error {
	content, err := streamProcessor.ExecStmt(stream)
	if err != nil {
		return fmt.Errorf("Stream command error: %s", err)
	} else {
		for _, c := range content {
			*reply = *reply + fmt.Sprintln(c)
		}
	}
	return nil
}

func (t *Server) CreateRule(rule *common.Rule, reply *string) error {
	r, err := ruleProcessor.ExecCreate(rule.Name, rule.Json)
	if err != nil {
		return fmt.Errorf("Create rule error : %s.", err)
	} else {
		*reply = fmt.Sprintf("Rule %s was created, please use 'cli getstatus rule $rule_name' command to get rule status.", rule.Name)
	}
	//Start the rule
	rs, err := t.createRuleState(r)
	if err != nil {
		return err
	}
	err = t.doStartRule(rs)
	if err != nil {
		return err
	}
	return nil
}

func (t *Server) createRuleState(rule *api.Rule) (*RuleState, error) {
	if tp, err := ruleProcessor.ExecInitRule(rule); err != nil {
		return nil, err
	} else {
		rs := &RuleState{
			Name:      rule.Id,
			Topology:  tp,
			Triggered: true,
		}
		registry[rule.Id] = rs
		return rs, nil
	}
}

func (t *Server) GetStatusRule(name string, reply *string) error {
	if rs, ok := registry[name]; ok {
		if !rs.Triggered {
			*reply = "Stopped: canceled manually."
			return nil
		}
		c := (*rs.Topology).GetContext()
		if c != nil {
			err := c.Err()
			switch err {
			case nil:
				keys, values := (*rs.Topology).GetMetrics()
				metrics := "{"
				for i, key := range keys {
					value := values[i]
					switch value.(type) {
					case string:
						metrics += fmt.Sprintf("\"%s\":%q,", key, value)
					default:
						metrics += fmt.Sprintf("\"%s\":%v,", key, value)
					}
				}
				metrics = metrics[:len(metrics)-1] + "}"
				dst := &bytes.Buffer{}
				if err = json.Indent(dst, []byte(metrics), "", "  "); err != nil {
					*reply = "Running with metrics:\n" + metrics
				} else {
					*reply = "Running with metrics:\n" + dst.String()
				}
			case context.Canceled:
				*reply = "Stopped: canceled by error."
			case context.DeadlineExceeded:
				*reply = "Stopped: deadline exceed."
			default:
				*reply = fmt.Sprintf("Stopped: %v.", err)
			}
		} else {
			*reply = "Stopped: no context found."
		}
	} else {
		return fmt.Errorf("Rule %s is not found", name)
	}
	return nil
}

func (t *Server) StartRule(name string, reply *string) error {
	var rs *RuleState
	rs, ok := registry[name]
	if !ok {
		r, err := ruleProcessor.GetRuleByName(name)
		if err != nil {
			return err
		}
		rs, err = t.createRuleState(r)
		if err != nil {
			return err
		}
	}
	err := t.doStartRule(rs)
	if err != nil {
		return err
	}
	*reply = fmt.Sprintf("Rule %s was started", name)
	return nil
}

func (t *Server) doStartRule(rs *RuleState) error {
	rs.Triggered = true
	go func() {
		tp := rs.Topology
		select {
		case err := <-tp.Open():
			tp.GetContext().SetError(err)
			log.Printf("closing rule %s for error: %v", rs.Name, err)
			tp.Cancel()
		}
	}()
	return nil
}

func (t *Server) StopRule(name string, reply *string) error {
	if rs, ok := registry[name]; ok {
		(*rs.Topology).Cancel()
		rs.Triggered = false
		*reply = fmt.Sprintf("Rule %s was stopped.", name)
	} else {
		*reply = fmt.Sprintf("Rule %s was not found.", name)
	}
	return nil
}

func (t *Server) RestartRule(name string, reply *string) error {
	err := t.StopRule(name, reply)
	if err != nil {
		return err
	}
	err = t.StartRule(name, reply)
	if err != nil {
		return err
	}
	*reply = fmt.Sprintf("Rule %s was restarted.", name)
	return nil
}

func (t *Server) DescRule(name string, reply *string) error {
	r, err := ruleProcessor.ExecDesc(name)
	if err != nil {
		return fmt.Errorf("Desc rule error : %s.", err)
	} else {
		*reply = r
	}
	return nil
}

func (t *Server) ShowRules(_ int, reply *string) error {
	r, err := ruleProcessor.ExecShow()
	if err != nil {
		return fmt.Errorf("Show rule error : %s.", err)
	} else {
		*reply = r
	}
	return nil
}

func (t *Server) DropRule(name string, reply *string) error {
	r, err := ruleProcessor.ExecDrop(name)
	if err != nil {
		return fmt.Errorf("Drop rule error : %s.", err)
	} else {
		err := t.StopRule(name, reply)
		if err != nil {
			return err
		}
	}
	*reply = r
	return nil
}

func init() {
	ticker := time.NewTicker(time.Second * 5)
	go func() {
		for {
			<-ticker.C
			if _, ok := registry[QUERY_RULE_ID]; !ok {
				continue
			}

			n := time.Now()
			w := 10 * time.Second
			if v := n.Sub(sinks.QR.LastFetch); v >= w {
				log.Printf("The client seems no longer fetch the query result, stop the query now.")
				stopQuery()
			}
		}
		//defer ticker.Stop()
	}()
}

func StartUp(Version string) {
	common.InitConf()

	dr, err := common.GetDataLoc()
	if err != nil {
		log.Panic(err)
	} else {
		log.Infof("db location is %s", dr)
		dataDir = dr
	}
	ruleProcessor = processors.NewRuleProcessor(path.Dir(dataDir))
	streamProcessor = processors.NewStreamProcessor(path.Join(path.Dir(dataDir), "stream"))

	registry = make(RuleRegistry)

	server := new(Server)
	//Start rules
	if rules, err := ruleProcessor.GetAllRules(); err != nil {
		log.Infof("Start rules error: %s", err)
	} else {
		log.Info("Starting rules")
		var reply string
		for _, rule := range rules {
			err = server.StartRule(rule, &reply)
			if err != nil {
				log.Info(err)
			} else {
				log.Info(reply)
			}
		}
	}

	//Start server
	err = rpc.Register(server)
	if err != nil {
		log.Fatal("Format of service Server isn't correct. ", err)
	}
	// Register a HTTP handler
	rpc.HandleHTTP()
	// Listen to TPC connections on port 1234
	listener, e := net.Listen("tcp", fmt.Sprintf(":%d", common.Config.Port))
	if e != nil {
		log.Fatal("Listen error: ", e)
	}
	msg := fmt.Sprintf("Serving kuiper (version - %s) on port %d... \n", Version, common.Config.Port)
	log.Info(msg)
	fmt.Printf(msg)
	if common.Config.Prometheus {
		go func() {
			port := common.Config.PrometheusPort
			if port <= 0 {
				log.Fatal("Miss configuration prometheusPort")
			}
			listener, e := net.Listen("tcp", fmt.Sprintf(":%d", port))
			if e != nil {
				log.Fatal("Listen prometheus error: ", e)
			}
			log.Infof("Serving prometheus metrics on port http://localhost:%d/metrics", port)
			http.Handle("/metrics", promhttp.Handler())
			http.Serve(listener, nil)
		}()
	}
	// Start accept incoming HTTP connections
	err = http.Serve(listener, nil)
	if err != nil {
		log.Fatal("Error serving: ", err)
	}
}
