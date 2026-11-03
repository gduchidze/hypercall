# Hypercall

Hypercall compares agent behavior across model and inference configurations.

## Language

**Whole-task experiment**:
A comparison in which each candidate performs the complete task from the same
initial inputs. Its results describe performance across the complete task.
_Avoid_: Continuation experiment when making whole-task reliability claims.

**Continuation experiment**:
A comparison in which candidates continue from a shared recorded point in an
agent's execution. Its results describe behavior after that point.
_Avoid_: Whole-task experiment when only the continuation was evaluated.
