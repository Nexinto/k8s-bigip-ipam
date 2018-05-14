# k8s-bigip-ipam

k8s-bigip-ipam adds generic IP address management support
for k8s-bigip-ctlr (https://github.com/F5Networks/k8s-bigip-ctlr).


## Overview

k8s-bigip-ipam uses k8s-bigip-ctlr's IPAM integration feature (http://clouddocs.f5.com/containers/v2/kubernetes/kctlr-manage-bigip-objects.html#attach-pools-to-a-virtual-server-using-ipam).
For a new Service, k8s-bigip-ipam will manage IP address reservation and create the configuration for k8s-bigip-ctlr.

As the end user, all you need to do is deploying a Service (type NodePort) and the loadbalancing setup will be created for you.
 
### Limitations

No support for Ingress or Route yet.

## Getting started

First, deploy k8s-bigip-ctlr (https://github.com/F5Networks/k8s-bigip-ctlr).

Choose an network that will contain the addresses for your VIPs.

Then, deploy a service to manage the IP address reservation for the virtual servers. Right now there are:

 * https://github.com/Nexinto/k8s-ipam-configmap (use this by default)
 * https://github.com/Nexinto/k8s-ipam-haci

When deploying one of those, one of the required configuration parameters is the network you are going to use for
your virtual services.

(Or, just deploy the custom resource from https://github.com/Nexinto/k8s-ipam/blob/master/deploy/crd.yaml and manually provide
IP addresses by using kubeipam, see https://github.com/Nexinto/k8s-ipam.)


Then, deploy k8s-bigip-ipam. Review the configuration parameters in `deploy/configmap.yaml`. The parameter `F5_PARTITION` must match the name of the F5 Partition you have
configured k8s-bigip-ctlr to manage.

Then, deploy all the files in deploy. 

```bash
kubectl apply -f deploy
```

You can also run the controller outside the Kubernetes cluster by setting the required environment variables.

## Configuration parameters

All configuration parameters are passed to the controller as environment variables:

| Variable | Description | Default |
|:-----|:------------|:--------|
|KUBECONFIG|your kubeconfig location (out of cluster only)||
|LOG_LEVEL|log level (debug, info, ...)|info|
|F5_PARTITION|The F5 Partition managed by k8s-bigip-ctlr|kubernetes|
|REQUIRE_TAG|Create loadbalancing only for Services with the annotation `nexinto.com/req-vip`|false|
|CONTROLLER_TAG|Set to a unique value if you are running multiple controller instances on the same F5|kubernetes|

## How to use it

By default, loadbalancing is created for every Service with type `NodePort`. If everything works, the IP of the
virtual server created for the Service is added as an annotation `nexinto.com/vip`:

```bash
kubectl describe service myservice
...
Annotations: ...
              nexinto.com/vip=10.160.10.161

```
(If you do not need loadbalancing for every service (of type `NodePort`), you can start the controller
with the configuration parameter `REQUIRE_TAG=true`. The controller will not create a virtual server
by default. Then, set the annotation `nexinto.com/req-vip` on all Services that require loadbalancing to `true`.
The Service still needs to have type `NodePort`.)

The addresses will be picked from the network you configured when you deployed one of the IP address
management services. If you create a new Service that needs to be loadbalanced, k8s-bigip-ipam will
create a new `ipaddress` request resource and waits for it to be processed. It then creates the
ConfigMap required by k8s-bigip-ipam; k8s-bigip-ipam will process the Service and the ConfigMap and
create the virtual server.

You can list those addresses using `kubectl get ipaddresses` and check details with `kubectl describe ipaddress ...`

### HTTP or TCP mode

The loadbalancing mode for your Service can be configured by setting the Annotation `nexinto.com/req-vip-mode` to
`tcp` or `http`. The default is `tcp`.

### SSL termination

If you would like BigIP to terminate your SSL connections, create an SSL profile on your BigIP and
set the Annotation `nexinto.com/vip-ssl-profiles` on your Service to the name the SSL profile.
Use the complete path for the profile, for example `Common/mysite`.

## Troubleshooting

If your virtual server isn't created, first check the Events for your Service (`kubectl describe service ...`)
and for the IP address resource (`kubectl describe ipaddress ...`; the name for the address is the same as your service).
The name of the created ConfigMap is `bigip-SERVICENAME-port`.

Then, check the logs of the k8s-bigip-ipam controller:

```bash
kubectl logs -f $(kubectl get pods -l app=k8s-bigip-ipam -n kube-system -o jsonpath='{.items[0].metadata.name}') -n kube-system
```

and the logs of k8s-bigip-ctlr:

```bash
kubectl logs -f $(kubectl get pods -l app=k8s-bigip-ctlr -n kube-system -o jsonpath='{.items[0].metadata.name}') -n kube-system
```

and the logs of the controller that provides the IP addresses, for example:

```bash
kubectl logs -f $(kubectl get pods -l app=k8s-ipam-configmap -n kube-system -o jsonpath='{.items[0].metadata.name}') -n kube-system
```
