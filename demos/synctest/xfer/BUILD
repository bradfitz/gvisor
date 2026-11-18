load("//tools:defs.bzl", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_test(
    name = "netstack_xfer_test",
    size = "large",
    srcs = [
        "netstack_test.go",
        "xfer_test.go",
    ],
    deps = [
        "//demos/synctest/simnet",
        "//demos/synctest/umltest",
        "//pkg/buffer",
        "//pkg/tcpip",
        "//pkg/tcpip/adapters/gonet",
        "//pkg/tcpip/link/channel",
        "//pkg/tcpip/link/ethernet",
        "//pkg/tcpip/network/arp",
        "//pkg/tcpip/network/ipv4",
        "//pkg/tcpip/stack",
        "//pkg/tcpip/transport/tcp",
        "//pkg/waiter",
    ],
)

# This test boots Linux kernels as child processes, which does not work
# in the Bazel sandbox. It skips itself unless UML_KERNEL is set; see
# ../README.md.
go_test(
    name = "linux_xfer_test",
    size = "large",
    srcs = [
        "linux_test.go",
        "xfer_test.go",
    ],
    data = ["//demos/synctest/guestinit"],
    tags = ["local"],
    deps = [
        "//demos/synctest/simnet",
        "//demos/synctest/uml",
        "//demos/synctest/umltest",
    ],
)
