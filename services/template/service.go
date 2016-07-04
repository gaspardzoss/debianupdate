package template

/*
The service.go defines what to do for each API-call. This part of the service
runs on the node.
*/

import (
	"time"

	"github.com/dedis/cothority/log"
	"github.com/dedis/cothority/network"
	"github.com/dedis/cothority/protocols/template"
	"github.com/dedis/cothority/sda"
)

// ServiceName is the name to refer to the Template service from another
// package.
const ServiceName = "Template"

func init() {
	sda.RegisterNewService(ServiceName, newTemplate)
}

// Template is our example-service
type Template struct {
	// We need to embed the ServiceProcessor, so that incoming messages
	// are correctly handled.
	*sda.ServiceProcessor
	path string
	// Count holds the number of calls to 'ClockRequest'
	Count int
}

// ClockRequest starts a template-protocol and returns the run-time.
func (t *Template) ClockRequest(e *network.ServerIdentity, req *ClockRequest) (network.Body, error) {
	t.Count += 1
	tree := req.Roster.GenerateBinaryTree()
	pi, err := t.CreateProtocolSDA(tree, template.Name)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	pi.Start()
	<-pi.(*template.ProtocolTemplate).ChildCount
	elapsed := time.Now().Sub(start).Seconds()
	return &ClockResponse{elapsed}, nil
}

// CountRequest returns the number of instantiations of the protocol.
func (t *Template) CountRequest(e *network.ServerIdentity, req *CountRequest) (network.Body, error) {
	return &CountResponse{t.Count}, nil
}

// NewProtocol is called on all nodes of a Tree (except the root, since it is
// the one starting the protocol) so it's the Service that will be called to
// generate the PI on all others node.
// If you use CreateProtocolSDA, this will not be called, as the SDA will
// instantiate the protocol on its own. If you need more control at the
// instantiation of the protocol, use CreateProtocolService, and you can
// give some extra-configuration to your protocol in here.
func (t *Template) NewProtocol(tn *sda.TreeNodeInstance, conf *sda.GenericConfig) (sda.ProtocolInstance, error) {
	log.Lvl3("Not templated yet")
	return nil, nil
}

// newTemplate receives the context and a path where it can write its
// configuration, if desired. As we don't know when the service will exit,
// we need to save the configuration on our own from time to time.
func newTemplate(c *sda.Context, path string) sda.Service {
	s := &Template{
		ServiceProcessor: sda.NewServiceProcessor(c),
		path:             path,
	}
	for _, req := range []interface{}{
		s.ClockRequest, s.CountRequest,
	} {
		err := s.RegisterMessage(req)
		if err != nil {
			log.ErrFatal(err, "Couldn't register message:")
		}
	}
	return s
}
