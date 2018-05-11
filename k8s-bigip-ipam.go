package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/Nexinto/k8s-lbutil"

	log "github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	ipamv1 "github.com/Nexinto/k8s-ipam/pkg/apis/ipam.nexinto.com/v1"
	ipamclientset "github.com/Nexinto/k8s-ipam/pkg/client/clientset/versioned"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"regexp"
	strconv "strconv"
	"strings"
)

const (

	// This annotation contains the VIP that is cofigured by the k8s-bigip-ctlr.
	AnnVirtualServerIP = "virtual-server.f5.com/ip"

	// The k8s-bigip-ctlr will set this to the VIP once it is configured on the loadbalancer.
	AnnVirtualServerIPStatus = "status.virtual-server.f5.com/ip"

	// This annotation selects one or more SSL profile
	AnnNxSSLProfiles = "nexinto.com/vip-ssl-profiles"

	// VIP Mode (http or tcp, http is the default)
	AnnNxVipMode = "nexinto.com/req-vip-mode"

	// bigip provider
	AnnNxVIPProviderBigIP = "bigip"
)

func main() {

	flag.Parse()

	// If this is not set, glog tries to log into something below /tmp which doesn't exist.
	flag.Lookup("log_dir").Value.Set("/")

	if e := os.Getenv("LOG_LEVEL"); e != "" {
		if l, err := log.ParseLevel(e); err == nil {
			log.SetLevel(l)
		} else {
			log.SetLevel(log.WarnLevel)
			log.Warnf("unknown log level %s, setting to 'warn'", e)
		}
	}

	var kubeconfig string

	if e := os.Getenv("KUBECONFIG"); e != "" {
		kubeconfig = e
	}

	clientConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	ipamclient, err := ipamclientset.NewForConfig(clientConfig)
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		panic(err.Error())
	}

	var partition, tag string

	if e := os.Getenv("F5_PARTITION"); e != "" {
		partition = e
	} else {
		partition = "kubernetes"
	}

	if e := os.Getenv("CONTROLLER_TAG"); e != "" {
		tag = e
	} else {
		tag = "kubernetes"
	}

	c := &Controller{
		Kubernetes: clientset,
		IpamClient: ipamclient,
		RequireTag: os.Getenv("REQUIRE_TAG") != "",
		Partition:  partition,
		Tag:        tag,
	}

	c.Initialize()
	c.Start()
}

func (c *Controller) ServiceCreatedOrUpdated(service *corev1.Service) error {
	log.Debugf("processing service '%s-%s'", service.Namespace, service.Name)

	ok, needsUpdate, newservice, err := lbutil.EnsureVIP(c.Kubernetes, c.IpamClient, c.IpAddressLister, service, AnnNxVIPProviderBigIP, c.RequireTag)
	if err != nil {
		return fmt.Errorf("error getting vip for service '%s-%s': %s", service.Namespace, service.Name, err.Error())
	} else if !ok {
		if needsUpdate {
			_, err = c.Kubernetes.CoreV1().Services(service.Namespace).Update(newservice)
			return err
		}
		return nil
	}

	var ssl bool
	var mode F5Mode

	if service.Annotations[AnnNxVipMode] == "http" {
		mode = F5ModeHTTP
	} else {
		mode = F5ModeTCP
	}

	if service.Annotations[AnnNxSSLProfiles] != "" {
		ssl = true
	} else {
		ssl = false
	}

	service = newservice

	ports := map[int32]bool{} // used for cleaning up later
	activeVips := 0
	wantedPorts := 0

	for _, port := range service.Spec.Ports {
		if port.Protocol == corev1.ProtocolUDP {
			continue
		}
		wantedPorts++
		ports[port.Port] = true
		mapname := configMapNameFor(service, port.Port)
		configMap, err := c.ConfigMapLister.ConfigMaps(service.Namespace).Get(mapname)
		if err == nil {
			uptodate, newConfigMap, reason := c.configMapUpToDate(service, configMap, ssl, mode, port.Port)
			if !uptodate {
				log.Infof("updating configmap '%s-%s' (%s)", configMap.Namespace, configMap.Name, reason)
				_, err = c.Kubernetes.CoreV1().ConfigMaps(service.Namespace).Update(newConfigMap)
				if err != nil {
					return err
				}
			} else if configMap.Annotations[AnnVirtualServerIPStatus] == service.Annotations[lbutil.AnnNxAssignedVIP] {
				activeVips++ // the bigip ctlr has created the correct VIP
			}
		} else {
			if !errors.IsNotFound(err) {
				return err
			}
			configMap = c.configMapFor(service, ssl, mode, port.Port)
			_, err = c.Kubernetes.CoreV1().ConfigMaps(service.Namespace).Create(configMap)
			if err != nil {
				return err
			}
			log.Infof("created configmap '%s-%s' for service '%s-%s' port %d", configMap.Namespace, configMap.Name, service.Namespace, service.Name, port.Port)
		}

	}

	if activeVips == wantedPorts && newservice.Annotations[lbutil.AnnNxVIP] != newservice.Annotations[lbutil.AnnNxAssignedVIP] {
		log.Infof("loadbalancing for service '%s-%s' is now ready with %d service port(s) on virtual IP '%s'", newservice.Namespace, service.Name, wantedPorts, newservice.Annotations[lbutil.AnnNxAssignedVIP])
		lbutil.MakeEvent(c.Kubernetes, service, fmt.Sprintf("Loadbalancing with virtual IP '%s' is ready with %d service port(s)", newservice.Annotations[lbutil.AnnNxAssignedVIP], wantedPorts), false)
		newservice.Annotations[lbutil.AnnNxVIP] = newservice.Annotations[lbutil.AnnNxAssignedVIP]
		_, err = c.Kubernetes.CoreV1().Services(newservice.Namespace).Update(newservice)
		if err != nil {
			return err
		}
	}

	// Clean up any leftover configmaps (for example, if the Ports of a Service were changed)

	r := regexp.MustCompile("bigip-([^-]+)-(.*)")
	configMaps, err := c.ConfigMapLister.List(labels.Everything())

	for _, configMap := range configMaps {
		if configMap.Namespace != service.Namespace {
			continue
		}
		if configMap.Labels["f5type"] != "virtual-server" {
			continue
		}

		m := r.FindStringSubmatch(configMap.Name)
		if len(m) != 3 {
			continue
		}

		if m[1] != service.Name {
			continue
		}

		servicePort, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}

		if ports[int32(servicePort)] {
			continue
		}
		log.Infof("deleting obsolete configmap '%s-%s'", configMap.Namespace, configMap.Name)
		err = c.Kubernetes.CoreV1().ConfigMaps(configMap.Namespace).Delete(configMap.Name, &metav1.DeleteOptions{})
		if err != nil {
			return err
		}
	}

	if needsUpdate {
		_, err = c.Kubernetes.CoreV1().Services(service.Namespace).Update(service)
		return err
	}

	return nil
}

func (c *Controller) ServiceDeleted(service *corev1.Service) error {
	log.Debugf("processing deleted service '%s-%s'", service.Namespace, service.Name)
	return nil
}

func (c *Controller) IpAddressCreatedOrUpdated(address *ipamv1.IpAddress) error {
	log.Debugf("processing address '%s-%s'", address.Namespace, address.Name)
	lbutil.IpAddressCreatedOrUpdated(c.ServiceQueue, address)
	return nil
}

func (c *Controller) IpAddressDeleted(address *ipamv1.IpAddress) error {
	log.Debugf("processing deleted address '%s-%s'", address.Namespace, address.Name)
	return lbutil.IpAddressDeleted(c.Kubernetes, c.ServiceLister, address)
}

func (c *Controller) ConfigMapCreatedOrUpdated(configMap *corev1.ConfigMap) error {
	log.Debugf("processing configmap '%s-%s'", configMap.Namespace, configMap.Name)

	if configMap.Labels["f5type"] != "virtual-server" {
		return nil
	}

	if configMap.Annotations[AnnVirtualServerIPStatus] != "" {
		// active loadbalancing

		for _, ref := range configMap.OwnerReferences {
			if ref.Kind == "Service" && ref.APIVersion == "v1" {
				log.Debugf("waking up service '%s-%s", configMap.Namespace, ref.Name)
				c.ServiceQueue.Add(configMap.Namespace + "/" + ref.Name)
			}
		}
	}

	return nil
}

func (c *Controller) mkF5Config(service *corev1.Service, ssl bool, mode F5Mode, port, servicePort int32) (f5 *F5VirtualServerConfig) {

	f5 = &F5VirtualServerConfig{
		VirtualServer: F5VirtualServer{
			Frontend: F5Frontend{
				Balance:        "round-robin",
				Mode:           mode,
				Partition:      c.Partition,
				VirtualAddress: F5VirtualAddress{Port: port},
			},
			Backend: F5Backend{ServiceName: service.Name, ServicePort: servicePort},
		},
	}

	if ssl {
		f5.VirtualServer.Frontend.SSLProfile = &F5SSLProfile{}
		ann := strings.Split(service.Annotations[AnnNxSSLProfiles], ",")

		if len(ann) == 1 {
			f5.VirtualServer.Frontend.SSLProfile.SSLProfileName = service.Annotations[AnnNxSSLProfiles]
		} else if len(ann) > 1 {
			f5.VirtualServer.Frontend.SSLProfile.SSLProfileNames = ann
		}
	}
	return
}

func (c *Controller) configMapFor(service *corev1.Service, ssl bool, mode F5Mode, servicePort int32) *corev1.ConfigMap {

	var port int32

	if mode == F5ModeTCP {
		port = servicePort
	} else {
		if ssl {
			port = 443
		} else {
			port = 80
		}
	}

	mapname := configMapNameFor(service, port)

	f5Config := c.mkF5Config(service, ssl, mode, port, servicePort)
	f5ConfigM, _ := json.Marshal(f5Config)

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      mapname,
			Namespace: service.Namespace,
			Labels:    map[string]string{"f5type": "virtual-server"},
			Annotations: map[string]string{
				AnnVirtualServerIP: service.Annotations[lbutil.AnnNxAssignedVIP],
			},
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "Service",
				APIVersion: "v1",
				Name:       service.Name,
				UID:        service.GetUID(),
			}},
		},
		Data: map[string]string{
			"schema": "f5schemadb://bigip-virtual-server_v0.1.3.json",
			"data":   string(f5ConfigM),
		},
	}
}

func (c *Controller) configMapUpToDate(service *corev1.Service, configMap *corev1.ConfigMap, ssl bool, mode F5Mode, servicePort int32) (bool, *corev1.ConfigMap, string) {
	var reason string
	wantedConfigMap := c.configMapFor(service, ssl, mode, servicePort)

	if configMap.Data["data"] != wantedConfigMap.Data["data"] {
		reason = "f5Config changed "
	}

	if configMap.Annotations[AnnVirtualServerIPStatus] != service.Annotations[lbutil.AnnNxAssignedVIP] {
		reason = reason + fmt.Sprintf("vip changes from %s to %s ", configMap.Annotations[AnnVirtualServerIPStatus], service.Annotations[lbutil.AnnNxAssignedVIP])
	}

	return reason == "", wantedConfigMap, reason
}

func configMapNameFor(service *corev1.Service, port int32) string {
	return fmt.Sprintf("bigip-%s-%d", service.Name, port)
}
