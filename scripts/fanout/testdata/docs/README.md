Dingo needs Go 1.26.0 to build.

The example Helm values use `ghcr.io/blinklabs-io/dingo:0.69.0`, enable all
API ports, and keep the Service internal.

Pull with `docker pull blinklabs/dingo:v0.69.0` or the adder image
`ghcr.io/blinklabs-io/adder:0.40.0`.

The example values pin Dingo `0.69.0`. Against 0.68.0 the endpoint is
unavailable.
