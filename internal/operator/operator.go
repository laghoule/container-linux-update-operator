package operator

import (
	"fmt"
	"time"

	"github.com/golang/glog"
	"k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/pkg/api"
	v1api "k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/fields"
	"k8s.io/client-go/pkg/util/flowcontrol"
	"k8s.io/client-go/pkg/watch"
	"k8s.io/client-go/tools/record"

	"github.com/coreos-inc/container-linux-update-operator/internal/constants"
	"github.com/coreos-inc/container-linux-update-operator/internal/k8sutil"
)

const (
	eventReasonRebootFailed = "RebootFailed"
	eventSourceComponent    = "update-operator"
)

var (
	// justRebootedSelector is a selector for combination of annotations
	// expected to be on a node after it has completed a reboot.
	//
	// The update-operator sets constants.AnnotationOkToReboot to true to
	// trigger a reboot, and the update-agent sets
	// constants.AnnotationRebootNeeded and
	// constants.AnnotationRebootInProgress to false when it has finished.
	justRebootedSelector = fields.Set(map[string]string{
		constants.AnnotationOkToReboot:       constants.True,
		constants.AnnotationRebootNeeded:     constants.False,
		constants.AnnotationRebootInProgress: constants.False,
	}).AsSelector()

	// wantsRebootSelector is a selector for the annotation expected to be on a node when it wants to be rebooted.
	//
	// The update-agent sets constants.AnnotationRebootNeeded to true when
	// it would like to reboot, and false when it starts up.
	//
	// If constants.AnnotationRebootPaused is set to "true", the update-agent will not consider it for rebooting.
	wantsRebootSelector = fields.ParseSelectorOrDie(constants.AnnotationRebootNeeded + "==" + constants.True + "," + constants.AnnotationRebootPaused + "!=" + constants.True)
)

type Kontroller struct {
	kc *kubernetes.Clientset
	nc v1core.NodeInterface
	er record.EventRecorder
}

func New() (*Kontroller, error) {
	// set up kubernetes in-cluster client
	kc, err := k8sutil.InClusterClient()
	if err != nil {
		return nil, fmt.Errorf("error creating Kubernetes client: %v", err)
	}

	// node interface
	nc := kc.Nodes()

	// create event emitter
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: kc.Events("")})
	er := broadcaster.NewRecorder(v1api.EventSource{Component: eventSourceComponent})

	return &Kontroller{kc, nc, er}, nil
}

func (k *Kontroller) Run() error {
	rl := flowcontrol.NewTokenBucketRateLimiter(0.2, 1)
	for {
		rl.Accept()

		nodelist, err := k.nc.List(v1api.ListOptions{})
		if err != nil {
			glog.Infof("Failed listing nodes %v", err)
			continue
		}

		nodes := k8sutil.FilterNodesByAnnotation(nodelist.Items, justRebootedSelector)

		if len(nodes) > 0 {
			glog.Infof("Found %d rebooted nodes, setting annotation %q to false", len(nodes), constants.AnnotationOkToReboot)
		}

		for _, n := range nodes {
			if err := k8sutil.SetNodeAnnotations(k.nc, n.Name, map[string]string{
				constants.AnnotationOkToReboot: constants.False,
			}); err != nil {
				glog.Infof("Failed setting annotation %q on node %q to false: %v", constants.AnnotationOkToReboot, n.Name, err)
			}
		}

		nodelist, err = k.nc.List(v1api.ListOptions{})
		if err != nil {
			glog.Infof("Failed listing nodes: %v", err)
			continue
		}

		nodes = k8sutil.FilterNodesByAnnotation(nodelist.Items, wantsRebootSelector)

		// pick N of these machines
		// TODO: for now, synchronous with N == 1. might be async w/ a channel in the future to handle N > 1
		if len(nodes) == 0 {
			continue
		}

		n := nodes[0]

		glog.Infof("Found %d nodes that need a reboot, rebooting %q", len(nodes), n.Name)

		k.handleReboot(&n)
	}
}

func (k *Kontroller) handleReboot(n *v1api.Node) {
	// node wants to reboot, so let it.
	if err := k8sutil.SetNodeAnnotations(k.nc, n.Name, map[string]string{
		constants.AnnotationOkToReboot: constants.True,
	}); err != nil {
		glog.Infof("Failed to set annotation %q on node %q: %v", constants.AnnotationOkToReboot, n.Name, err)
		return
	}

	// wait for it to come back...
	watcher, err := k.nc.Watch(v1api.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("metadata.name", n.Name).String(),
		ResourceVersion: n.ResourceVersion,
	})

	conds := []watch.ConditionFunc{
		k8sutil.NodeAnnotationCondition(constants.AnnotationOkToReboot, constants.True),
		k8sutil.NodeAnnotationCondition(constants.AnnotationRebootNeeded, constants.False),
		k8sutil.NodeAnnotationCondition(constants.AnnotationRebootInProgress, constants.False),
	}
	_, err = watch.Until(time.Hour*1, watcher, conds...)
	if err != nil {
		glog.Infof("Waiting for label %q on node %q failed: %v", constants.AnnotationOkToReboot, n.Name, err)
		glog.Infof("Failed to wait for successful reboot of node %q", n.Name)

		k.er.Eventf(n, api.EventTypeWarning, eventReasonRebootFailed, "Timed out waiting for node to return after a reboot")
	}

	// node rebooted successfully, or at least set the labels we expected from klocksmith after a reboot.
}
