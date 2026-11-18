load("//tools:defs.bzl", "go_binary")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_binary(
    name = "guestinit",
    srcs = ["main.go"],
    pure = True,
    static = True,
    visibility = ["//demos/synctest:__subpackages__"],
    deps = ["@org_golang_x_sys//unix:go_default_library"],
)
