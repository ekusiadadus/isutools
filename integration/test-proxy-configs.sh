#!/usr/bin/env bash
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
proxy_dir="$repo_root/examples/proxies"
validation_dir=$(mktemp -d "${TMPDIR:-/tmp}/isutools-proxy-configs.XXXXXX")

cleanup() {
  case "$validation_dir" in
    "${TMPDIR:-/tmp}"/isutools-proxy-configs.*) rm -rf -- "$validation_dir" ;;
    *) echo "refusing to remove unexpected validation path: $validation_dir" >&2 ;;
  esac
}
trap cleanup EXIT

command -v docker >/dev/null
command -v ruby >/dev/null
install -d -m 0777 "$validation_dir/envoy-log"

ruby -ryaml - "$proxy_dir" "$validation_dir" <<'RUBY'
proxy_dir, output_dir = ARGV

%w[traefik.yaml trafficserver-logging.yaml].each do |file|
  YAML.safe_load(File.read(File.join(proxy_dir, file)))
end

nginx_fragment = File.read(File.join(proxy_dir, "nginx.conf"))
nginx_config = <<~NGINX
  worker_processes 1;
  pid /tmp/nginx.pid;
  error_log stderr;
  events {}
  http {
  #{nginx_fragment.lines.map { |line| "  #{line}" }.join}
    server {
      listen 8080;
      location / { return 204; }
    }
  }
NGINX
File.write(File.join(output_dir, "nginx.conf"), nginx_config)

h2o_fragment = YAML.safe_load(File.read(File.join(proxy_dir, "h2o.conf")))
h2o_config = {
  "listen" => 8080,
  "hosts" => {"default" => {"paths" => {"/" => {"file.dir" => "/tmp"}}}},
}.merge(h2o_fragment)
File.write(File.join(output_dir, "h2o.yaml"), YAML.dump(h2o_config))

def envoy_bootstrap(access_log)
  {
    "static_resources" => {
      "listeners" => [{
        "name" => "listener",
        "address" => {"socket_address" => {"address" => "0.0.0.0", "port_value" => 8080}},
        "filter_chains" => [{"filters" => [{
          "name" => "envoy.filters.network.http_connection_manager",
          "typed_config" => {
            "@type" => "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
            "stat_prefix" => "ingress_http",
            "codec_type" => "AUTO",
            "route_config" => {
              "name" => "local_route",
              "virtual_hosts" => [{
                "name" => "app",
                "domains" => ["*"],
                "routes" => [{"match" => {"prefix" => "/"}, "route" => {"cluster" => "app"}}],
              }],
            },
            "http_filters" => [{
              "name" => "envoy.filters.http.router",
              "typed_config" => {"@type" => "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"},
            }],
            "access_log" => access_log,
          },
        }]}],
      }],
      "clusters" => [{
        "name" => "app",
        "connect_timeout" => "0.25s",
        "type" => "STATIC",
        "load_assignment" => {
          "cluster_name" => "app",
          "endpoints" => [{"lb_endpoints" => [{
            "endpoint" => {"address" => {"socket_address" => {"address" => "127.0.0.1", "port_value" => 3000}}},
          }]}],
        },
      }],
    },
  }
end

{"envoy" => "envoy.yaml", "envoy-legacy" => "envoy-legacy.yaml"}.each do |name, file|
  fragment = YAML.safe_load(File.read(File.join(proxy_dir, file)))
  File.write(File.join(output_dir, "#{name}.yaml"), YAML.dump(envoy_bootstrap(fragment.fetch("access_log"))))
end
RUBY

echo "validating nginx 1.28.0"
docker run --rm \
  -v "$validation_dir/nginx.conf:/etc/nginx/nginx.conf:ro" \
  nginx:1.28.0-alpine nginx -t

echo "validating Apache httpd 2.4.65"
docker run --rm \
  -v "$proxy_dir/apache.conf:/work/apache.conf:ro" \
  httpd:2.4.65-alpine sh -ec '
    cp /usr/local/apache2/conf/httpd.conf /tmp/httpd.conf
    sed -i "s/^#LoadModule headers_module/LoadModule headers_module/" /tmp/httpd.conf
    printf "\nInclude /work/apache.conf\n" >> /tmp/httpd.conf
    mkdir -p /var/log/apache2
    httpd -t -f /tmp/httpd.conf
  '

echo "validating H2O 2.2.5"
docker run --rm \
  -v "$validation_dir/h2o.yaml:/work/h2o.yaml:ro" \
  ubuntu:24.04 bash -ec '
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq h2o=2.2.5+dfsg2-8.1ubuntu3 >/dev/null
    h2o --mode=test --conf /work/h2o.yaml
  '

echo "validating Envoy 1.34.13"
docker run --rm \
  -v "$validation_dir/envoy.yaml:/work/envoy.yaml:ro" \
  -v "$validation_dir/envoy-log:/var/log/envoy" \
  envoyproxy/envoy:v1.34.13 --mode validate --config-path /work/envoy.yaml

echo "validating legacy Envoy 1.33.11"
docker run --rm \
  -v "$validation_dir/envoy-legacy.yaml:/work/envoy.yaml:ro" \
  -v "$validation_dir/envoy-log:/var/log/envoy" \
  envoyproxy/envoy:v1.33.11 --mode validate --config-path /work/envoy.yaml

echo "validating Caddy 2.10.2"
docker run --rm \
  -v "$proxy_dir/Caddyfile:/etc/caddy/Caddyfile:ro" \
  caddy:2.10.2-alpine caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
