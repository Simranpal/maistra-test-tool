// Copyright 2021 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package certificate

import (
	"fmt"
	"os"
	"testing"

	"github.com/maistra/maistra-test-tool/pkg/app"
	"github.com/maistra/maistra-test-tool/pkg/util"
	"github.com/maistra/maistra-test-tool/pkg/util/check/assert"
	"github.com/maistra/maistra-test-tool/pkg/util/curl"
	"github.com/maistra/maistra-test-tool/pkg/util/env"
	"github.com/maistra/maistra-test-tool/pkg/util/oc"
	"github.com/maistra/maistra-test-tool/pkg/util/pod"
	"github.com/maistra/maistra-test-tool/pkg/util/retry"
	"github.com/maistra/maistra-test-tool/pkg/util/shell"
	"github.com/maistra/maistra-test-tool/pkg/util/test"
)

func TestExternalCertificate(t *testing.T) {
	test.NewTest(t).Id("T17").Groups(test.Full, test.ARM, test.InterOp).Run(func(t test.TestHelper) {
		const ns = "bookinfo"

		var tmpDir = ""

		t.Cleanup(func() {
			if tmpDir != "" {
				os.RemoveAll(tmpDir)
			}

			oc.RecreateNamespace(t, ns, meshNamespace)
		})

		t.LogStep("Uninstall existing SMCP")
		oc.RecreateNamespace(t, meshNamespace)

		t.LogStep("Create cacerts Secret")
		oc.CreateGenericSecretFromFiles(t, meshNamespace, "cacerts",
			"ca-cert.pem="+sampleCACert,
			"ca-key.pem="+sampleCAKey,
			"root-cert.pem="+sampleCARoot,
			"cert-chain.pem="+sampleCAChain)

		t.LogStep("Apply SMCP to configure certificate authority to use cacerts Secret")
		oc.ApplyString(t, meshNamespace, createSMCPWithCustomCert(smcpName, ns))
		oc.WaitSMCPReady(t, meshNamespace, smcpName)

		t.LogStep("Install bookinfo")
		app.InstallAndWaitReady(t, app.BookinfoWithMTLS(ns))

		t.LogStep("Checking response from productpage.")
		retry.UntilSuccess(t, func(t test.TestHelper) {
			curl.Request(t,
				app.BookinfoProductPageURL(t, meshNamespace), nil,
				assert.ResponseStatus(200))
		})

		if env.GetArch() == "p" || env.GetArch() == "z" {
			t.Log("NOTE: Not checking certificates, because test is running in P or Z environment")
			return
		}

		t.LogStep("Retrieve certificates using the 'openssl s_client -showcerts' command")
		tmpDir = shell.CreateTempDir(t, "cacerts")
		oc.Exec(t, pod.MatchingSelector("app=productpage", ns), "istio-proxy",
			fmt.Sprintf(`openssl s_client -showcerts -connect details:9080 > '%s/bookinfo-proxy-cert.txt' || true`, tmpDir))

		t.LogStep("Extract certificates")
		shell.Executef(t,
			`sed -n '/-----BEGIN CERTIFICATE-----/{:start /-----END CERTIFICATE-----/!{N;b start};/.*/p}' '%s/bookinfo-proxy-cert.txt' > '%s/certs.pem'`,
			tmpDir, tmpDir)
		shell.Executef(t,
			`awk 'BEGIN {counter=0;} /BEGIN CERT/{counter++} { print > "%s/proxy-cert-" counter ".pem"}' < '%s/certs.pem'`,
			tmpDir, tmpDir)

		t.NewSubTest("root certificate").Run(func(t test.TestHelper) {
			shell.Executef(t, `openssl x509 -in '%s' -text -noout > '%s/root-cert.crt.txt'`, sampleCARoot, tmpDir)
			shell.Executef(t, `openssl x509 -in '%s/proxy-cert-3.pem' -text -noout > '%s/pod-root-cert.crt.txt'`, tmpDir, tmpDir)

			if err := util.CompareFiles(fmt.Sprintf("%s/root-cert.crt.txt", tmpDir), fmt.Sprintf("%s/pod-root-cert.crt.txt", tmpDir)); err != nil {
				t.Errorf("Root certs do not match: %v", err)
			} else {
				t.LogSuccess("Root certificate received from pod matches the root certificate in cacerts")
			}
		})

		t.NewSubTest("CA certificate").Run(func(t test.TestHelper) {
			shell.Executef(t, `openssl x509 -in '%s' -text -noout > '%s/ca-cert.crt.txt'`, sampleCACert, tmpDir)
			shell.Executef(t, `openssl x509 -in '%s/proxy-cert-2.pem' -text -noout > '%s/pod-cert-chain-ca.crt.txt'`, tmpDir, tmpDir)

			if err := util.CompareFiles(fmt.Sprintf("%s/ca-cert.crt.txt", tmpDir), fmt.Sprintf("%s/pod-cert-chain-ca.crt.txt", tmpDir)); err != nil {
				t.Errorf("CA certs do not match: %v", err)
			} else {
				t.LogSuccess("CA certificate received from pod matches the CA certificate in cacerts")
			}
		})

		t.NewSubTest("certificate chain").Run(func(t test.TestHelper) {
			shell.Executef(t, `cat '%s' '%s' > '%s/sample-cert-and-root-cert.pem'`, sampleCACert, sampleCARoot, tmpDir)
			shell.Execute(t,
				fmt.Sprintf(`openssl verify -CAfile '%s/sample-cert-and-root-cert.pem' '%s/proxy-cert-1.pem'`, tmpDir, tmpDir),
				assert.OutputContains(fmt.Sprintf("%s/proxy-cert-1.pem: OK", tmpDir),
					"Certificate chain verified.",
					"Certificate chain could not be verified."))
		})
	})
}

func createSMCPWithCustomCert(smcpName, memberNs string) string {
	return fmt.Sprintf(`apiVersion: maistra.io/v2
kind: ServiceMeshControlPlane
metadata:
  name: %s
spec:
  addons:
    grafana:
      enabled: false
    kiali:
      enabled: false
    prometheus:
      enabled: false
  gateways:
    egress:
      enabled: false
    openshiftRoute:
      enabled: false
  security:
    dataPlane:
      mtls: true
    certificateAuthority:
      type: Istiod
      istiod:
        type: PrivateKey
        privateKey:
          rootCADir: /etc/cacerts
  tracing:
    type: None
  version: %v
---
apiVersion: maistra.io/v1
kind: ServiceMeshMemberRoll
metadata:
  name: default
spec:
  members:
  - %s
`, smcpName, env.GetSMCPVersion(), memberNs)
}
