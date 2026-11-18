load("//tools:defs.bzl", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "umltest",
    testonly = 1,
    srcs = ["umltest.go"],
    visibility = ["//demos/synctest:__subpackages__"],
    deps = [
        "//demos/synctest/initramfs",
        "//demos/synctest/uml",
        "//pkg/test/testutil",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
