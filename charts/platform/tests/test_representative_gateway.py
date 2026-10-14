"""Offline render checks; no cluster, credentials, or Helm plugins needed.

Run: python3 -m unittest discover -s charts/platform/tests -v
Requires Helm on PATH, or HELM=/path/to/helm. Missing Helm is a failed
prerequisite rather than a passing/skipped security test.
"""

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


CHART = Path(__file__).resolve().parents[1]


class RepresentativeGatewayChartTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.helm = shutil.which(os.environ.get("HELM", "helm"))
        if cls.helm is None:
            raise RuntimeError("Helm is required for chart render tests (set HELM or PATH)")

    def render(self, gateway=None, error=None):
        with tempfile.TemporaryDirectory() as directory:
            values = Path(directory) / "values.json"
            values.write_text(json.dumps({"representativeGateway": gateway} if gateway is not None else {}))
            result = subprocess.run(
                [self.helm, "template", "test", str(CHART), "--namespace", "test-system", "-f", str(values)],
                text=True, capture_output=True, check=False,
            )
        if error is not None:
            self.assertNotEqual(result.returncode, 0, result.stdout)
            # Helm versions report schema paths as dotted names or JSON
            # pointers; assert the same field without depending on that UI.
            self.assertIn(error, result.stderr.replace("/", "."))
        else:
            self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout

    @staticmethod
    def resource(rendered, kind, name):
        for document in re.split(r"^---\s*$", rendered, flags=re.MULTILINE):
            if re.search(rf"^kind: {re.escape(kind)}$", document, re.MULTILINE) and re.search(
                rf"^metadata:\n  name: {re.escape(name)}$", document, re.MULTILINE
            ):
                return document
        raise AssertionError(f"Missing {kind}/{name}")

    @staticmethod
    def enabled(**kwargs):
        return {"enabled": True, "existingSecret": "gateway-test", **kwargs}

    def test_disabled_by_default(self):
        rendered = self.render()
        self.assertNotIn("REPRESENTATIVE_CONFIG_FILE", rendered)
        self.assertNotIn("REPRESENTATIVE_LISTEN_ADDR", rendered)
        self.assertNotIn("name: steadmesh-representative-gateway", rendered)
        self.assertNotIn("representative-config", rendered)
        self.assertNotIn("containerPort: 8082", rendered)
        self.assertNotIn("kind: Ingress", rendered)

    def test_requires_existing_secret(self):
        self.render({"enabled": True}, error="representativeGateway.existingSecret is required")

    def test_enabled_projects_secret_without_creating_one(self):
        rendered = self.render(self.enabled())
        deployment = self.resource(rendered, "Deployment", "steadmesh-platform")
        self.assertIn("name: REPRESENTATIVE_CONFIG_FILE", deployment)
        self.assertIn("value: /etc/steadmesh/representative/gateway.json", deployment)
        self.assertIn('value: ":8082"', deployment)
        self.assertIn("containerPort: 8082", deployment)
        self.assertIn("fsGroup: 65532", deployment)
        self.assertIn("defaultMode: 0440", deployment)
        self.assertIn("projected:", deployment)
        self.assertIn('name: "gateway-test"', deployment)
        self.assertIn("key: gateway.json", deployment)
        self.assertIn("path: gateway.json", deployment)
        self.assertIn("mountPath: /etc/steadmesh/representative", deployment)
        self.assertIn("readOnly: true", deployment)
        self.assertNotRegex(deployment, r"(?m)^\s+subPath:")
        self.assertNotIn("kind: Secret", rendered)
        self.assertNotIn("kind: Ingress", rendered)

    def test_dedicated_service_does_not_expose_internal_port(self):
        rendered = self.render(self.enabled())
        service = self.resource(rendered, "Service", "steadmesh-representative-gateway")
        self.assertIn("type: ClusterIP", service)
        self.assertIn("app.kubernetes.io/name: steadmesh-platform", service)
        self.assertIn("port: 8082", service)
        self.assertIn("targetPort: representative", service)
        self.assertNotIn("8080", "\n".join(line for line in service.splitlines() if not line.startswith("#")))
        self.assertNotIn("targetPort: http", service)
        self.assertEqual(len(re.findall(r"(?m)^    - name:", service)), 1)

    def test_internal_service_remains_unchanged(self):
        before = self.resource(self.render(), "Service", "steadmesh-platform")
        after = self.resource(self.render(self.enabled()), "Service", "steadmesh-platform")
        self.assertEqual(before, after)

    def test_custom_port_is_consistent(self):
        rendered = self.render(self.enabled(port=18082))
        self.assertIn('value: ":18082"', rendered)
        self.assertIn("containerPort: 18082", rendered)
        self.assertIn("port: 18082", self.resource(rendered, "Service", "steadmesh-representative-gateway"))

    def test_rejects_internal_and_invalid_ports(self):
        for port in [8080, 0, -1, 80, 65536, 8082.5, "bad", "8080:8082"]:
            with self.subTest(port=port):
                self.render(self.enabled(port=port), error="representativeGateway.port")

    def test_enable_flags_must_be_booleans(self):
        self.render({"enabled": "false"}, error="representativeGateway.enabled")
        self.render(self.enabled(ingress={"enabled": "false"}), error="representativeGateway.ingress.enabled")

    def test_inline_credentials_are_not_supported_values(self):
        self.render(self.enabled(bearerToken="test-value"), error="bearerToken")
        self.render(self.enabled(gatewayConfig={"principals": []}), error="gatewayConfig")

    def test_ingress_cannot_enable_gateway_implicitly(self):
        self.render({"ingress": {"enabled": True}}, error="representativeGateway.enabled must be true")

    def test_ingress_requires_host(self):
        self.render(self.enabled(ingress={"enabled": True, "tlsSecretName": "gateway-tls"}),
                    error="representativeGateway.ingress.host is required")

    def test_ingress_requires_tls(self):
        self.render(self.enabled(ingress={"enabled": True, "host": "representative.example.com"}),
                    error="HTTPS is mandatory")

    def test_ingress_has_only_exact_gateway_paths_and_tls(self):
        rendered = self.render(self.enabled(ingress={
            "enabled": True, "host": "representative.example.com", "tlsSecretName": "gateway-tls",
            "className": "test-ingress", "annotations": {"example.com/test": "enabled"},
        }))
        ingress = self.resource(rendered, "Ingress", "steadmesh-representative-gateway")
        self.assertIn('ingressClassName: "test-ingress"', ingress)
        self.assertIn("example.com/test: enabled", ingress)
        self.assertIn('secretName: "gateway-tls"', ingress)
        self.assertIn('host: "representative.example.com"', ingress)
        self.assertIn('- "representative.example.com"', ingress)
        self.assertEqual(re.findall(r'^\s+- path: "([^"]+)"$', ingress, re.MULTILINE), [
            "/mcp", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp",
        ])
        self.assertEqual(ingress.count("pathType: Exact"), 3)
        self.assertEqual(ingress.count("name: steadmesh-representative-gateway"), 4)
        self.assertEqual(ingress.count("name: representative"), 3)
        self.assertNotIn("pathType: Prefix", ingress)
        self.assertNotIn("defaultBackend:", ingress)
        self.assertNotIn("name: steadmesh-platform", ingress)


if __name__ == "__main__":
    unittest.main()
