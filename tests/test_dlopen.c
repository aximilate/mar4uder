#include <stdio.h>
#include <dlfcn.h>

int main(void) {
    void *h = dlopen("libX11.so.6", RTLD_LAZY);
    printf("dlopen: %p, err: %s\n", h, dlerror());
    return 0;
}
