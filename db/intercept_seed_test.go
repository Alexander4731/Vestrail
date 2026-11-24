package db

import (
	"regexp"
	"testing"
)

// 内置「删除类接口路径」规则匹配的是整个 tool_input JSON 串，因此用例直接以
// JSON 形态给出，与 Interceptor 实际拿到的 subject 一致。
func TestDeleteEndpointPathPattern(t *testing.T) {
	re := regexp.MustCompile(deleteEndpointPathPattern)

	hit := []string{
		`{"command":"curl -s 'http://t.com/api/user/delete?id=1'"}`,    // GET 打删除接口
		`{"command":"curl -X POST http://t.com/admin/delete -d id=1"}`, // POST 打删除接口
		`{"command":"curl 'http://t.com/api/deleteAll'"}`,
		`{"command":"curl 'http://t.com/api/delete_user?id=1'"}`,
		`{"command":"curl 'http://t.com/api/delete-user?id=1'"}`,
		`{"url":"http://t.com/api/remove?id=1"}`,
		`{"command":"curl http://t.com/files/unlink/3"}`,
		`{"command":"curl http://t.com/api/del?id=2"}`,
		`{"command":"curl -X POST http://t/v1/erase"}`,
		`{"command":"curl http://t/admin/destroyAll"}`, // v1 的路径规则不允许后缀，这里补上
	}
	for _, s := range hit {
		if !re.MatchString(s) {
			t.Errorf("应命中却放行: %s", s)
		}
	}

	// 动词后必须跟分隔符，避免 /delivery、/details 这类只读路径被误拦。
	miss := []string{
		`{"command":"curl 'http://t.com/api/delivery?id=1'"}`,
		`{"command":"curl 'http://t.com/order/details'"}`,
		`{"command":"curl 'http://t.com/api/delta/sync'"}`,
		`{"command":"curl 'http://t.com/user/delegate'"}`,
		`{"command":"curl 'http://delete.example.com/'"}`, // 删除动词出现在域名而非路径
		`{"command":"curl 'http://t.com/remote/status'"}`,
		`{"command":"nmap -p80 10.0.0.1"}`,
	}
	for _, s := range miss {
		if re.MatchString(s) {
			t.Errorf("误拦: %s", s)
		}
	}
}

func TestV5HighRiskPatterns(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		hits    []string
		misses  []string
	}{
		{
			name:    "infraDestroyV5",
			pattern: infraDestroyV5Pattern,
			hits: []string{
				`{"command":"pulumi destroy --yes"}`,
				`{"command":"kubectl delete ns prod"}`,
				`{"command":"kubectl delete namespace staging"}`,
				`{"command":"docker system prune -af"}`,
				`{"command":"docker volume rm pgdata"}`,
			},
			misses: []string{
				`{"command":"kubectl get pods -n prod"}`,
				`{"command":"docker ps -a"}`,
			},
		},
		{
			name:    "factoryResetV5",
			pattern: factoryResetV5Pattern,
			hits: []string{
				`{"content":"('POST','api/cluster/restoreFactorySettings','{}')"}`,
				`{"url":"https://t/api/sys/factoryReset"}`,
				`{"url":"https://t/api/device/resetFactory"}`,
				`{"url":"https://t/api/config/restoreDefault"}`,
				`{"url":"https://t/api/sys/resetToDefault"}`,
				`{"url":"https://t/api/cluster/resetAll"}`,
				`{"url":"https://t/api/sys/reinit"}`,
			},
			misses: []string{
				`{"command":"curl https://t/api/user/list"}`,
				`{"command":"curl https://t/api/sys/getDefaultLang"}`,
			},
		},
		{
			name:    "serviceDisruptV5",
			pattern: serviceDisruptV5Pattern,
			hits: []string{
				`{"content":"('POST','api/sysParam/restartTomcat','{}')"}`,
				`{"content":"('POST','api/cluster/restartCluster','{}')"}`,
				`{"content":"('POST','api/devAgent/batchStopService','{}')"}`,
				`{"command":"systemctl stop nginx"}`,
				`{"command":"Stop-Service Spooler"}`,
			},
			misses: []string{
				`{"command":"systemctl status nginx"}`,
				`{"command":"curl https://t/api/sysParam/getTime"}`,
			},
		},
		{
			name:    "uninstallV5",
			pattern: uninstallV5Pattern,
			hits: []string{
				`{"url":"https://t/api/plugin/uninstallAll"}`,
				`{"url":"https://t/api/agent/removeAgent"}`,
				`{"command":"apt-get purge -y nginx"}`,
				`{"command":"rpm -e openssh-server"}`,
				`{"command":"helm uninstall prod-app"}`,
				`{"command":"msiexec /x {1234} /qn"}`,
			},
			misses: []string{
				`{"command":"apt-cache policy nginx"}`,
				`{"command":"helm list"}`,
			},
		},
		{
			name:    "firmwarePatchRestoreV5",
			pattern: firmwarePatchRestoreV5Pattern,
			hits: []string{
				`{"content":"('POST','api/patch/installForClusterNode','{}')"}`,
				`{"url":"https://t/api/sysParam/restoreConfiguration"}`,
				`{"url":"https://t/api/device/flashFirmware"}`,
				`{"url":"https://t/api/cluster/deployNode"}`,
			},
			misses: []string{
				`{"url":"https://t/api/sysParam/getConfigurationBackupList"}`,
			},
		},
		{
			name:    "accountTamperV5",
			pattern: accountTamperV5Pattern,
			hits: []string{
				`{"url":"https://t/api/user/resetAdminPassword"}`,
				`{"url":"https://t/api/user/disableUser/1"}`,
				`{"command":"passwd root"}`,
				`{"command":"userdel -r admin"}`,
				`{"command":"net user bob /delete"}`,
			},
			misses: []string{
				`{"url":"https://t/api/user/current"}`,
				`{"command":"cat /etc/passwd"}`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			re := regexp.MustCompile(tc.pattern)
			for _, h := range tc.hits {
				if !re.MatchString(h) {
					t.Errorf("%s should match %s", tc.name, h)
				}
			}
			for _, m := range tc.misses {
				if re.MatchString(m) {
					t.Errorf("%s false-positived on %s", tc.name, m)
				}
			}
		})
	}
}
