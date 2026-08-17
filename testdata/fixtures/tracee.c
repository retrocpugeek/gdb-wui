#include <stdio.h>
#include <sys/prctl.h>
#include <time.h>
#include <unistd.h>

/*
 * A process to attach to: it belongs to whoever started it, and the point of
 * the test that uses it is that it is still running afterwards.
 *
 * PR_SET_PTRACER_ANY is what makes the test runnable at all. The default
 * kernel.yama.ptrace_scope of 1 — on developer machines and on the CI runners
 * both — permits tracing only a descendant, and gdb here is the test's child
 * rather than this process's parent, so a sibling could not attach without it.
 * A program that expects to be attached to does exactly this; the alternative
 * is asking every reader to run a sysctl before the tests pass.
 *
 * Bounded by wall-clock time for the same reason threads.c is: an inferior that
 * outlives the gdb attached to it must die by itself rather than wait for a
 * test to remember it.
 */
#define LIFETIME_SECONDS 60

int counter;

int main(void)
{
	time_t deadline = time(NULL) + LIFETIME_SECONDS;

	prctl(PR_SET_PTRACER, PR_SET_PTRACER_ANY, 0, 0, 0);

	/* Printed after the prctl, so a reader of this line knows attaching is
	 * permitted; the pid itself comes from the process that started us. */
	printf("ready\n");
	fflush(stdout);

	while (time(NULL) < deadline) {
		counter++;
		sleep(1);
	}
	return 0;
}
