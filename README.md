
# k8s-cgroup-burst-controller

Simple app that updates CPU burst setting using containerd interface

This application is inspired by [kubernetes-cfs-burst](https://github.com/christiancadieux/kubernetes-cfs-burst/tree/main)
but the way it works is very different.
`kubernetes-cfs-burst` works only with cgroup v1,
and it sets cpu burst for each container in the namespace as a percentage relative to CPU limit.

This application requires separate settings per pod,
defines burst in seconds,
and allows you to define different absolute burst value for each container.
It is tested with cgroup v2, but should theoretically also work with cgroup v1.

# Read before use

This is a workaround for a missing feature in k8s 1.32.
Future versions of k8s may include support for CPU burst, making this app obsolete.
Track feature request status here:
[Use Linux CFS burst to get rid of unnecessary CPU throttling](https://github.com/kubernetes/kubernetes/issues/104516)

Since this is a workaround, it's not guaranteed to work perfectly.
I did my best to make it work as best as possible but I may have missed something.

Requirements:
- runc v1.2.0 or newer
- containerd 2.0 or newer
- Linux kernel 5.14 or newer

A major issue with the cgroup burst itself is bad support in mainstream linux kernel.
If you want to fix throttling, you _will_ need to install a custom kernel.
See details [below](#cgroup-burst-support-in-linux-kernel)

# Installation

```bash

kubectl create ns cgroup-burst
# cgroup-burst needs access to hostPath volumes to access containerd directly
kubectl label ns cgroup-burst pod-security.kubernetes.io/enforce=privileged

kubectl apply -f https://github.com/d-uzlov/k8s-cgroup-burst-operator/raw/refs/heads/main/deployment/rbac.yaml
kubectl apply -f https://github.com/d-uzlov/k8s-cgroup-burst-operator/raw/refs/heads/main/deployment/daemonset.yaml

# since realistically you need a custom kernel to benefit from this app
# the example deployment defines a node selector
# label all nodes that you want it to be running on
node=
kubectl label node $node cgroup.meoe.io/node=enable

# check that everything is working
kubectl -n cgroup-burst get pod -o wide

```

# Usage example

Enable burst for a specific pod:

```bash

# select a pod
pod_namespace=
pod_name=

# choose a container in the pod
container_name=
# set burst value
# burst is measured in seconds
# you can use SI-compatible suffixes
burst_time=100ms

kubectl -n $pod_namespace annotate pod $pod_name cgroup.meoe.io/burst=$container_name=$burst_time
kubectl -n $pod_namespace label pod $pod_name cgroup.meoe.io/burst=enable
# check pod events for errors and info
kubectl -n $pod_namespace describe pod $pod_name

```

If more than one container in the pod needs burst, then you can set values for each of them:
`container-1=10ms,container-2=20ms,container-3=30ms`

For persistent usage add metadata to the pod that you want to enable burst on:

```yaml
metadata:
  labels:
    cgroup.meoe.io/burst: enable
  annotations:
    cgroup.meoe.io/burst: nginx=10ms
```

For deployments, daemonsets, etc., add metadata to pod template.

# How to verify if burst is working

You can check cAdvisor metrics: `rate(container_cpu_cfs_throttled_periods_total)`.
After applying burst settings the rate should consistently drop.
But depending on your application you may need to set a relatively large burst to reduce the rate to 0.

You can check if the burst is applied to the cgroup if you have SSH access to the node the pod is running on:

```bash

# find id of the container
sudo crictl ps

# check container metadata
sudo crictl inspect 8bf89cb991fba | jq .info.runtimeSpec.linux.resources

# check real cgroup
sudo crictl inspect 8bf89cb991fba | jq .info.pid
# get cgroup path from the main container process
cat /proc/2853468/cgroup
cd /sys/fs/cgroup/
# substitute path from /proc/.../cgroup
cd kubepods.slice/kubepods-burstable.slice/.../....scope
cat ./cpu.max
cat ./cpu.max.burst
cat ./cpu.stat

```

# Known errors

Cgroup burst settings may fail to apply:

```
updateTask: runc did not terminate successfully:
exit status 1: failed to write \"10000\":
write /sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod645c13ba_18c3_46dd_a446_05c675532185.slice/cri-containerd-f513cab41c3a2736244036009794202db825dcddba6ff7f5284246e91c304d11.scope/cpu.max.burst:
invalid argument\n: unknown
```

You need to install a [custom linux kernel](#install-patched-linux-kernel-on-debian) to solve this.
See [Cgroup burst support in linux kernel](#cgroup-burst-support-in-linux-kernel) for reasoning.

# Metrics

App provides several types of metrics:
- Own metrics. Prefixed with `cgroup_burst_`. Served on `/metrics`
- Pod spec metrics. Tries to mimic kube-state-metrics. Served on `/container_metrics`
- - `kube_pod_container_cgroup_burst`
- Cgroup statistics. Tries to mimic cAdvisor metrics. Served on `/container_metrics`
- - `container_cpu_cgroup_burst_periods_total`
- - `container_cpu_cgroup_burst_seconds_total`
- Standard Golang metrics. Served on `/default_metrics`

Cgroup statistics require access to host cgroup info, and work only with cgroup v2.
Safer way to find it is to decode cgroup info from container spec. Example deployment is configured to use this method.
However, the format seems to be unstable, so it can theoretically break with updates.

Alternative (and more robust) way is to use data from `/proc` but it requires container to run in privileged mode.
Here is an example of deployment that uses `/proc`: [daemonset-proc-metrics.yaml](./deployment/daemonset-proc-metrics.yaml)

# Cgroup burst support in linux kernel

CFS CPU burst support was added to Linux kernel in the 5.14 version:
https://github.com/torvalds/linux/commit/f4183717b370ad28dd0c0d74760142b20e6e7931

However, it artificially limits max burst amount to quota size:
https://github.com/torvalds/linux/blob/f4183717b370ad28dd0c0d74760142b20e6e7931/kernel/sched/core.c#L9814

And this limit is still present in the 6.15:
https://github.com/torvalds/linux/blob/v6.15-rc2/kernel/sched/core.c#L9477

This is very stupid, because quota and burst are measured in different units:
quota is in seconds per scheduling period, and the burst is in absolute seconds.
If you set quota to 0.5 cores, and the period is 100 ms (default value in k8s),
then burst is limited to 50 ms, and if the period is 10 ms, then burst is limited to 5 ms.
The limit is absolutely artificial, and does not make sense to me in any way.

But more importantly, if your application sleeps most of the time, and the average CPU usage is `1m`,
you would want to set CPU limit to some small value. For example, to `5m`.
Then your burst will be limited to _astounding_ and _mind blowing_ 0.5 ms!
This will surely completely solve all your app throttling issues!

Seriously though, limit `burst <= quota` effectively allows you to just double CPU limit, but no more than that.

So, while you can use this application on mainstream kernels, it's not very useful.

To make better use of this feature, you need to use a [custom Linux kernel](#install-patched-linux-kernel-on-debian) which was patched to disable this limit.

P.S. In various old articles about burst in cgroup v1 I see a reference to `sched_cfs_bw_burst_enabled` kernel parameter,
that is supposedly needed to bypass the `burst <= quota` limit.
However, I'm not able to find any details about this parameter in the internet,
and I don't see anything related to it in the mainstream kernel code.
From what I can tell, the `burst <= quota` check was in the kernel from the first commit of the burst feature,
and there are no ways to bypass this, regardless of any kernel parameters.

# Building

```bash

# local testing
CGO_ENABLED=0 go build .
./cgroup-burst

# build image for deployment
docker build .

image_name=k8s-cgroup-burst-controller:v0.2.11

docker_username=
docker build --push . -t docker.io/$docker_username/$image_name

github_username=
docker build --push . -t ghcr.io/$github_username/$image_name

```

# Install patched linux kernel on Debian

This is a kernel with a patch to unlock unlimited values for CPU burst feature.

Instructions to build the kernel can be found [here](./kernel-build.md#building-debian-12-kernel-61222-from-backports)

```bash

# check if this kernel is already installed
sudo dpkg --list | grep 6.12.22-burstunlock

mkdir linux-6.12.22-burstunlock0
cd linux-6.12.22-burstunlock0
wget https://github.com/d-uzlov/k8s-cgroup-burst-controller/releases/download/kernel-debian-6.12/6.12.22-burstunlock0.zip
unzip 6.12.22-burstunlock0.zip

sudo apt install -y libdw1 pahole gcc-12 binutils

sudo dpkg -i *-burstunlock0-*.deb

rm 6.12.22-burstunlock0.zip

# if you want to remove this kernel
sudo dpkg --list | grep 6.12.22-burstunlock0 | awk '{ print $2 }' | xargs sudo dpkg -P

```
