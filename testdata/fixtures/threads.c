#include <pthread.h>
#include <stdio.h>
#include <time.h>
#include <unistd.h>

/*
 * The program a dozen integration tests run and then interrupt, pause, kill or
 * question while it is still running.
 *
 * The workers are bounded by wall-clock time, not by an iteration count. A
 * counted loop is a race: the original one finished in under 10ms, so every
 * "while running" test was betting that its next round-trip beat the program
 * to the exit, and CI eventually lost that bet.
 *
 * The bound exists only so an inferior that outlives its gdb dies by itself;
 * no test waits for it. The sleep keeps three of these off three cores.
 */
#define LIFETIME_SECONDS 60

static pthread_barrier_t barrier;

static void *worker(void *arg)
{
	long id = (long)arg;
	time_t deadline = time(NULL) + LIFETIME_SECONDS;
	int spins = 0;

	pthread_barrier_wait(&barrier);
	while (time(NULL) < deadline) {
		spins++;
		if (spins % 1000 == 0)
			usleep(1000);
	}
	printf("worker %ld done\n", id);
	return NULL;
}

int main(void)
{
	pthread_t t[3];
	long i;

	pthread_barrier_init(&barrier, NULL, 4);
	for (i = 0; i < 3; i++)
		pthread_create(&t[i], NULL, worker, (void *)i);
	pthread_barrier_wait(&barrier);
	for (i = 0; i < 3; i++)
		pthread_join(t[i], NULL);
	pthread_barrier_destroy(&barrier);
	return 0;
}
