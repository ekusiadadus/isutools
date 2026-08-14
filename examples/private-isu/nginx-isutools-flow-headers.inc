# Include this inside the proxying location, after any other proxy_set_header
# directives. nginx does not inherit parent proxy_set_header directives when
# the location declares one of its own.
# Closed k6 benchmark only: the app still requires the explicit trusted-input
# opt-in and validates both values before returning them to nginx.
proxy_set_header X-Isutools-Trusted-Session $http_x_isutools_session;
proxy_set_header X-Isutools-Trusted-Scenario $http_x_isutools_scenario;
proxy_set_header X-Isutools-Session "";
proxy_set_header X-Isutools-Scenario "";
proxy_hide_header X-Isutools-Session;
proxy_hide_header X-Isutools-Scenario;
