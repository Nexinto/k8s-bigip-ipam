package main

import (
	"encoding/json"
	"fmt"
	ipamfake "github.com/Nexinto/k8s-ipam/pkg/client/clientset/versioned/fake"
	"github.com/Nexinto/k8s-lbutil"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"testing"
	"time"
)

// Create a test environment with some useful defaults.
func testEnvironment() *Controller {

	log.SetLevel(log.DebugLevel)

	c := &Controller{
		Kubernetes: fake.NewSimpleClientset(),
		IpamClient: ipamfake.NewSimpleClientset(),
		RequireTag: false,
	}

	c.Kubernetes.CoreV1().Namespaces().Create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}})

	c.Kubernetes.CoreV1().Nodes().Create(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Address: "10.100.11.1", Type: corev1.NodeInternalIP}}},
	})
	c.Kubernetes.CoreV1().Nodes().Create(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node2"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Address: "10.100.11.2", Type: corev1.NodeInternalIP}}},
	})
	c.Kubernetes.CoreV1().Nodes().Create(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node3"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Address: "10.100.11.3", Type: corev1.NodeInternalIP}}},
	})

	c.Initialize()
	go c.Start()

	stopCh := make(chan struct{})

	go c.Run(stopCh)

	log.Debug("waiting for cache sync")

	if !cache.WaitForCacheSync(stopCh, c.ServiceSynced, c.ConfigMapSynced, c.IpAddressSynced) {
		panic("Timed out waiting for caches to sync")
	}

	return c
}

// Simulates the behaviour of the ipam controller.
func (c *Controller) simIPAM() error {
	i := 1

	addrs, err := c.IpamClient.IpamV1().IpAddresses(metav1.NamespaceAll).List(metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, addr := range addrs.Items {
		if addr.Status.Address == "" {
			addr.Status.Address = fmt.Sprintf("10.0.0.%d", i)
			i++
			_, err := c.IpamClient.IpamV1().IpAddresses(addr.Namespace).Update(&addr)

			log.Debugf("[simIPAM] assign: %s/%s -> %s", addr.Namespace, addr.Name, addr.Status.Address)

			if err != nil {
				return err
			}
		}
	}

	return nil
}

// Simulate what the k8s-BigIP controller would do - add the
// status.virtual-server.f5.com/ip annotation to the relevant configmaps.
func (c *Controller) simBigIpCtlr() error {
	configMaps, _ := c.ConfigMapLister.ConfigMaps(metav1.NamespaceAll).List(labels.Everything())
	for _, configMap := range configMaps {
		if configMap.Annotations != nil &&
			configMap.Annotations[AnnVirtualServerIP] != "" &&
			configMap.Annotations[AnnVirtualServerIPStatus] == "" {

			newConfigMap := configMap.DeepCopy()

			newConfigMap.Annotations[AnnVirtualServerIPStatus] =
				newConfigMap.Annotations[AnnVirtualServerIP]

			log.Debugf("[simBigIpCtlr] configuring vip %s for '%s-%s'", newConfigMap.Annotations[AnnVirtualServerIPStatus], configMap.Namespace, configMap.Name)

			_, err := c.Kubernetes.CoreV1().ConfigMaps(newConfigMap.Namespace).Update(newConfigMap)
			if err != nil {
				return err
			}

		}
	}
	return nil
}

func TestDefaultLifecycle(t *testing.T) {
	c := testEnvironment()
	a := assert.New(t)

	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "myservice",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{
				{
					Port:     80,
					NodePort: 33978,
				},
				{
					Port:     443,
					NodePort: 32156,
				},
			},
		},
	}

	s, err := c.Kubernetes.CoreV1().Services("default").Create(s)
	if !a.Nil(err) {
		return
	}

	// This isn't what it looks like.
	time.Sleep(2 * time.Second)

	err = c.simIPAM()
	if !a.Nil(err) {
		return
	}

	// You don't see this
	time.Sleep(2 * time.Second)

	ia, err := c.IpamClient.IpamV1().IpAddresses("default").Get("myservice", metav1.GetOptions{})
	if !a.Nil(err) {
		return
	}

	assigned := ia.Status.Address
	if !a.NotEmpty(assigned) {
		return
	}

	err = c.simBigIpCtlr()
	if !a.Nil(err) {
		return
	}

	// Not happening
	time.Sleep(2 * time.Second)

	s, err = c.Kubernetes.CoreV1().Services("default").Get("myservice", metav1.GetOptions{})
	if !a.Nil(err) {
		return
	}

	if !a.Equal(assigned, s.Annotations[lbutil.AnnNxVIP]) {
		return
	}

	cm80, err := c.Kubernetes.CoreV1().ConfigMaps("default").Get("bigip-myservice-80", metav1.GetOptions{})
	if !a.Nil(err) {
		return
	}

	if !a.NotNil(cm80) {
		return
	}

	var vServer F5VirtualServerConfig

	err = json.Unmarshal([]byte(cm80.Data["data"]), &vServer)
	a.Nil(err)

	a.Empty(vServer.VirtualServer.Frontend.VirtualAddress.BindAddr)
	a.Equal(int32(80), vServer.VirtualServer.Frontend.VirtualAddress.Port)

	a.Equal("myservice", vServer.VirtualServer.Backend.ServiceName)
	a.Equal(int32(80), vServer.VirtualServer.Backend.ServicePort)
}

// Test that we can remove a Port
func TestRemovePort(t *testing.T) {

	c := testEnvironment()
	a := assert.New(t)

	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "myservice",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{
				{
					Port:     80,
					NodePort: 33978,
				},
				{
					Port:     443,
					NodePort: 32156,
				},
			},
		},
	}

	s, err := c.Kubernetes.CoreV1().Services("default").Create(s)
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	err = c.simIPAM()
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	err = c.simBigIpCtlr()
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	// Remove port 80

	s.Spec.Ports = []corev1.ServicePort{
		{
			Port:     443,
			NodePort: 32156,
		},
	}

	s, err = c.Kubernetes.CoreV1().Services("default").Update(s)
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	_, err = c.Kubernetes.CoreV1().ConfigMaps("default").Get("bigip-myservice-80", metav1.GetOptions{})
	a.NotNil(err)
	a.True(errors.IsNotFound(err))

	cm443, err := c.Kubernetes.CoreV1().ConfigMaps("default").Get("bigip-myservice-443", metav1.GetOptions{})
	a.Nil(err)
	a.NotNil(cm443)
}

// Test that we can add a Port
func TestAddPort(t *testing.T) {

	c := testEnvironment()
	a := assert.New(t)

	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "myservice",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{
				{
					Port:     80,
					NodePort: 33978,
				},
			},
		},
	}

	s, err := c.Kubernetes.CoreV1().Services("default").Create(s)
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	err = c.simIPAM()
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	err = c.simBigIpCtlr()
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	// Add port 443

	s.Spec.Ports = []corev1.ServicePort{
		{
			Port:     80,
			NodePort: 33978,
		},
		{
			Port:     443,
			NodePort: 32156,
		},
	}

	s, err = c.Kubernetes.CoreV1().Services("default").Update(s)
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	cm80, err := c.Kubernetes.CoreV1().ConfigMaps("default").Get("bigip-myservice-80", metav1.GetOptions{})
	a.Nil(err)
	a.NotNil(cm80)

	cm443, err := c.Kubernetes.CoreV1().ConfigMaps("default").Get("bigip-myservice-443", metav1.GetOptions{})
	a.Nil(err)
	a.NotNil(cm443)
}

// Test that we can change a Port
func TestChangePort(t *testing.T) {

	c := testEnvironment()
	a := assert.New(t)

	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "myservice",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{
				{
					Port:     80,
					NodePort: 33978,
				},
			},
		},
	}

	s, err := c.Kubernetes.CoreV1().Services("default").Create(s)
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	err = c.simIPAM()
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	err = c.simBigIpCtlr()
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	// Change port 80 to 443

	s.Spec.Ports = []corev1.ServicePort{
		{
			Port:     443,
			NodePort: 32156,
		},
	}

	s, err = c.Kubernetes.CoreV1().Services("default").Update(s)
	if !a.Nil(err) {
		return
	}

	time.Sleep(2 * time.Second)

	_, err = c.Kubernetes.CoreV1().ConfigMaps("default").Get("bigip-myservice-80", metav1.GetOptions{})
	a.NotNil(err)
	a.True(errors.IsNotFound(err))

	cm443, err := c.Kubernetes.CoreV1().ConfigMaps("default").Get("bigip-myservice-443", metav1.GetOptions{})
	a.Nil(err)
	a.NotNil(cm443)
}
