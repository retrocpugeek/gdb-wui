#include <pthread.h>
#include <stdio.h>
#include <unistd.h>

static pthread_barrier_t barrier;

static void *worker(void *arg)
{
	long id = (long)arg;
	int spins = 0;

	pthread_barrier_wait(&barrier);
	while (spins < 1000000) {
		spins++;
		if (spins % 250000 == 0)
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
