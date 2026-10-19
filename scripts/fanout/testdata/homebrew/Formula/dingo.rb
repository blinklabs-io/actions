class Dingo < Formula
  desc "Cardano data node built in Go"
  homepage "https://github.com/blinklabs-io/dingo"
  url "https://github.com/blinklabs-io/dingo/archive/refs/tags/v0.83.1.tar.gz"
  sha256 "bfee32645c8997fe5cb72ce516152046f1c8d71ec43586a24323cf338849c612"
  license "Apache-2.0"

  def install
    ldflags = %W[
      -s -w
      -X github.com/blinklabs-io/dingo/internal/version.Version=v#{version}
      -X github.com/blinklabs-io/dingo/internal/version.CommitHash=ad0c4b9
    ]
  end
end
