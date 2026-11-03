//go:build !linux

package hostdoctor

import "context"

func platformChecks(_ context.Context, _ Options) []Check {
	return []Check{
		blocked("host.kvm", "KVM requires Linux; run this command inside the Linux development VM"),
		blocked("host.cgroups", "cgroups v2 requires Linux"),
		blocked("host.namespaces", "Linux mount/network/PID/IPC/UTS namespaces are required"),
	}
}
