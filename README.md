
# aws-ondemand-proxy

Simple TCP proxy app that will start ec2 instance in case of external requests and stop when no traffic

The basic idea from knative scaling to zero.

# How it works

Very similar to Knative but dead simple

```
ExternalIP/ELB -> ondemand-proxy -> ec2 instance
```

When a connection comes to ondemand-proxy, it holds it up, while in the background starts to scale the target app.
When the target app is ready, traffic will be proxied.
When no traffic comes to the proxy in an idle period, 15 minutes by default, the proxied app will be scaled to 0.

Example
[![asciicast](https://asciinema.org/a/ZlIbWm8UQv4yVaOqfm3IzUPoT.svg)](https://asciinema.org/a/ZlIbWm8UQv4yVaOqfm3IzUPoT)
