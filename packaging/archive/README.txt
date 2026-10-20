xScaler Collector
=================

xscaler-collector    the xScaler OpenTelemetry Collector distribution
xscaler-supervisor   the OpenTelemetry OpAMP supervisor that enrolls with
                     xScaler and runs xscaler-collector with the configuration
                     assigned to this agent

On Linux, prefer the deb/rpm packages (apt/yum repository at
https://packages.xscaler.io) and on Windows the MSI: both install a service.

To run from this archive, put your enrollment token in the environment and
start the supervisor from this directory:

  export XSCALER_ENROLLMENT_TOKEN=xse_...
  ./xscaler-supervisor --config config/supervisor.yaml

On Windows, change agent::executable in config/supervisor.yaml to
.\xscaler-collector.exe first.

Docs: https://github.com/xscaler/xscaler-agent
