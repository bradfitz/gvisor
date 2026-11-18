load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "uml",
    srcs = [
        "nic.go",
        "timetravel.go",
        "vhostuser.go",
        "vm.go",
    ],
    visibility = ["//demos/synctest:__subpackages__"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)

# This test boots Linux kernels as child processes, which does not work
# in the Bazel sandbox. It skips itself unless UML_KERNEL is set; see
# ../README.md.
go_test(
    name = "uml_test",
    size = "large",
    srcs = ["uml_test.go"],
    data = ["//demos/synctest/guestinit"],
    tags = ["local"],
    deps = [
        ":uml",
        "//demos/synctest/umltest",
    ],
)
