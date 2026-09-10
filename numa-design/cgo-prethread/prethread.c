// prethread.c: a C constructor starts a pthread BEFORE the Go runtime
// initializes. That thread later calls into Go (goWork) and allocates.
#define _GNU_SOURCE
#include <errno.h>
#include <pthread.h>
#include <stdio.h>
#include <sys/syscall.h>
#include <unistd.h>

extern void goWork(void);

static pthread_t th;
static volatile int goReady;

static void *thr(void *arg) {
	while (!__atomic_load_n(&goReady, __ATOMIC_ACQUIRE))
		usleep(1000);
	goWork();
	return NULL;
}

__attribute__((constructor)) static void startPrethread(void) {
	pthread_create(&th, NULL, thr, NULL);
}

void markReady(void) { __atomic_store_n(&goReady, 1, __ATOMIC_RELEASE); }
void joinPrethread(void) { pthread_join(th, NULL); }

// taskPolicyMode returns the calling thread's task memory policy mode via
// get_mempolicy(2) with no flags: 0=MPOL_DEFAULT, 1=PREFERRED, 2=BIND, ...
// or -errno.
int taskPolicyMode(void) {
	int mode = -1;
	long r = syscall(SYS_get_mempolicy, &mode, NULL, 0, NULL, 0);
	return r != 0 ? -errno : mode;
}
long tid(void) { return syscall(SYS_gettid); }
